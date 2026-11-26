package pki

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matfax/hvmc/api/v1alpha1"
	"github.com/matfax/hvmc/internal/policy"
)

// Enrollment endpoint (DESIGN §4.6); the wire format is in enrollwire.go.
//
// A request is checked in this order, and the first failing check decides
// the status:
//
//  1. method POST (405) and a bearer token in the Authorization header
//     (401);
//  2. the body: at most MaxEnrollRequestBytes of JSON with exactly the
//     fields of EnrollRequest, a DNS-1123 agent name, a known role and a
//     PEM CSR with a valid ECDSA P-256 self-signature (400);
//  3. the agent is declared in HostConfig (404);
//  4. the token matches the agent's bootstrap Secret, is unexpired and
//     unused (401);
//  5. the role is the agent's role in HostConfig, the CSR's subject CN is
//     the agent name, and every SAN the CSR asks for is one HostConfig
//     lists for the agent (403).
//
// Steps 3 and 4 always read both HostConfig and the agent's Secret and
// compare the token in constant time (against a dummy when there is no
// Secret), so the time of a refusal does not tell more about the agent
// than its status code. The certificate is signed with the SANs from
// HostConfig only ([AgentIdentity]); the token is consumed afterwards by
// an update conditional on the Secret's resourceVersion, so a concurrent
// second use of the token gets 401. Tokens and request bodies are never
// logged.

// MaxEnrollRequestBytes bounds the body of an enrollment request.
const MaxEnrollRequestBytes = 64 << 10

// maxTokenLength bounds the bearer token read from the header.
const maxTokenLength = 512

// EnrollHandler serves POST /v1/enroll. It is an http.Handler for the :8080
// listener (DESIGN §8) and is safe for concurrent use.
type EnrollHandler struct {
	// Client reads HostConfig, Nodes (for node-mapped VMs) and bootstrap
	// Secrets and updates the latter. Its scheme must include corev1 and
	// v1alpha1. A cached client is fine: the token is consumed with an
	// update conditional on the resourceVersion that was read.
	Client client.Client
	// Namespace is the release namespace holding the bootstrap Secrets.
	Namespace string
	// CA signs the agents' serving certificates.
	CA *CA
	// Clock is the time source for token expiry; nil means SystemClock.
	Clock Clock
	// Log receives one line per refused or failed request; nil discards.
	Log *slog.Logger
}

// enrollError is a refusal with its HTTP status. msg goes to the client;
// cause, if any, only to the log.
type enrollError struct {
	status int
	msg    string
	cause  error
}

func (e *enrollError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %v", e.msg, e.cause)
	}
	return e.msg
}

func refuse(status int, msg string) *enrollError { return &enrollError{status: status, msg: msg} }

func internalError(msg string, cause error) *enrollError {
	return &enrollError{status: http.StatusInternalServerError, msg: msg, cause: cause}
}

// errTokenRefused is the one message for every token refusal, so the body
// does not tell a wrong token from an expired or used one.
const errTokenRefused = "bootstrap token is missing, wrong, expired or already used"

// ServeHTTP implements http.Handler.
func (h *EnrollHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	agent, resp, err := h.enroll(w, r)
	if err == nil {
		writeEnrollJSON(w, http.StatusOK, resp)
		h.log().LogAttrs(r.Context(), slog.LevelInfo, "agent enrolled", slog.String("agent", agent))
		return
	}
	var ee *enrollError
	if !errors.As(err, &ee) {
		ee = internalError("enrollment failed", err)
	}
	level := slog.LevelWarn
	if ee.status >= http.StatusInternalServerError {
		level = slog.LevelError
	}
	h.log().LogAttrs(r.Context(), level, "enrollment refused",
		slog.String("agent", agent), slog.Int("status", ee.status), slog.String("reason", ee.Error()),
		slog.String("remote", r.RemoteAddr))
	switch ee.status {
	case http.StatusUnauthorized:
		w.Header().Set("WWW-Authenticate", "Bearer")
	case http.StatusMethodNotAllowed:
		w.Header().Set("Allow", http.MethodPost)
	}
	writeEnrollJSON(w, ee.status, EnrollErrorResponse{Error: ee.msg})
}

// enroll handles one request and returns the agent name (for logs, empty
// if the body was not understood) and the response.
func (h *EnrollHandler) enroll(w http.ResponseWriter, r *http.Request) (string, *EnrollResponse, error) {
	if r.Method != http.MethodPost {
		return "", nil, refuse(http.StatusMethodNotAllowed, "method not allowed")
	}
	token, ok := bearerToken(r)
	if !ok {
		return "", nil, refuse(http.StatusUnauthorized, errTokenRefused)
	}
	req, csr, err := decodeEnrollRequest(w, r)
	if err != nil {
		return "", nil, err
	}
	ctx := r.Context()
	hc, agent, secret, err := h.authenticate(ctx, req.Agent, token)
	if err != nil {
		return req.Agent, nil, err
	}
	resp, err := h.issue(ctx, hc, agent, req.Role, csr)
	if err != nil {
		return req.Agent, nil, err
	}
	if err := h.consume(ctx, secret); err != nil {
		return req.Agent, nil, err
	}
	return req.Agent, resp, nil
}

// bearerToken returns the token of an `Authorization: Bearer` header.
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != "" && len(token) <= maxTokenLength
}

// decodeEnrollRequest reads and validates the body (step 2).
func decodeEnrollRequest(w http.ResponseWriter, r *http.Request) (*EnrollRequest, *x509.CertificateRequest, error) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxEnrollRequestBytes))
	dec.DisallowUnknownFields()
	var req EnrollRequest
	if err := dec.Decode(&req); err != nil {
		return nil, nil, &enrollError{status: http.StatusBadRequest, msg: "malformed request body", cause: err}
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, nil, refuse(http.StatusBadRequest, "malformed request body: trailing data")
	}
	if errs := validation.IsDNS1123Label(req.Agent); len(errs) > 0 {
		return nil, nil, refuse(http.StatusBadRequest, "agent must be a DNS-1123 label")
	}
	if req.Role != EnrollRoleHost && req.Role != EnrollRoleGuest {
		return nil, nil, refuse(http.StatusBadRequest, fmt.Sprintf("role must be %q or %q", EnrollRoleHost, EnrollRoleGuest))
	}
	csr, err := parseEnrollCSR(req.CSR)
	if err != nil {
		return nil, nil, &enrollError{status: http.StatusBadRequest, msg: "csr must be a PEM certificate request signed by an ECDSA P-256 key", cause: err}
	}
	return &req, csr, nil
}

// parseEnrollCSR parses a PEM CSR and checks its self-signature and key.
func parseEnrollCSR(s string) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("no PEM CERTIFICATE REQUEST block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, err
	}
	if pub, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P256() {
		return nil, errors.New("public key is not ECDSA P-256")
	}
	return csr, nil
}

// authenticate performs steps 3 and 4 and returns HostConfig, the agent and
// its bootstrap Secret.
func (h *EnrollHandler) authenticate(ctx context.Context, name, token string) (*v1alpha1.HostConfig, declaredAgent, *corev1.Secret, error) {
	var hc v1alpha1.HostConfig
	err := h.Client.Get(ctx, types.NamespacedName{Name: v1alpha1.HostConfigName}, &hc)
	if client.IgnoreNotFound(err) != nil {
		return nil, declaredAgent{}, nil, internalError("cannot read HostConfig", err)
	}
	agent, declared := lookupAgent(&hc, name)
	// Read the Secret and compare the token whether or not the agent is
	// declared, so both refusals take the same work.
	secret, valid, err := h.checkToken(ctx, name, token)
	if err != nil {
		return nil, declaredAgent{}, nil, err
	}
	if !declared {
		return nil, declaredAgent{}, nil, refuse(http.StatusNotFound, "agent is not declared in HostConfig")
	}
	if !valid {
		return nil, declaredAgent{}, nil, refuse(http.StatusUnauthorized, errTokenRefused)
	}
	return &hc, agent, secret, nil
}

// dummyToken is compared against when an agent has no usable token.
var dummyToken = sha256.Sum256([]byte("hvmc: no bootstrap token"))

// checkToken reads the agent's Secret and reports whether token is its
// current, unexpired, unused token.
func (h *EnrollHandler) checkToken(ctx context.Context, agent, token string) (*corev1.Secret, bool, error) {
	var s corev1.Secret
	key := types.NamespacedName{Namespace: h.Namespace, Name: BootstrapSecretName(agent)}
	err := h.Client.Get(ctx, key, &s)
	if client.IgnoreNotFound(err) != nil {
		return nil, false, internalError("cannot read bootstrap token", err)
	}
	want := dummyToken
	if stored := s.Data[BootstrapTokenKey]; err == nil && len(stored) > 0 {
		want = sha256.Sum256(stored)
	}
	got := sha256.Sum256([]byte(token))
	match := subtle.ConstantTimeCompare(got[:], want[:]) == 1
	usable := err == nil && tokenUsable(&s, h.now())
	return &s, match && usable, nil
}

// issue performs step 5, signs the certificate and builds the response.
func (h *EnrollHandler) issue(ctx context.Context, hc *v1alpha1.HostConfig, agent declaredAgent, role string, csr *x509.CertificateRequest) (*EnrollResponse, error) {
	if role != agent.role {
		return nil, refuse(http.StatusForbidden, fmt.Sprintf("agent %q has role %q", agent.name, agent.role))
	}
	if csr.Subject.CommonName != agent.name {
		return nil, refuse(http.StatusForbidden, "csr common name must be the agent name")
	}
	id, err := AgentIdentity(ctx, h.Client, hc, agent.name)
	if err != nil {
		return nil, internalError("cannot derive the agent's certificate names from HostConfig", err)
	}
	if err := checkRequestedSANs(csr, id); err != nil {
		return nil, err
	}
	cert, err := h.CA.SignCSR(csr.Raw, id)
	if err != nil {
		return nil, internalError("cannot sign certificate", err)
	}
	resp := &EnrollResponse{
		Certificate:   string(EncodeCertificatePEM(cert)),
		CACertificate: string(h.CA.CertPEM()),
		Settings:      AgentSettings{Port: agent.port},
	}
	if agent.role == EnrollRoleHost {
		resp.Policy = string(policy.FromHostConfig(hc).Canonical())
	}
	return resp, nil
}

// checkRequestedSANs refuses a CSR that asks for a SAN HostConfig does not
// list for the agent. The certificate never carries the CSR's SANs; this
// only tells an agent that asks for the wrong names.
func checkRequestedSANs(csr *x509.CertificateRequest, id Identity) error {
	if len(csr.URIs) > 0 || len(csr.EmailAddresses) > 0 {
		return refuse(http.StatusForbidden, "csr asks for URI or email SANs")
	}
	for _, d := range csr.DNSNames {
		if !slices.Contains(id.DNSNames, d) {
			return refuse(http.StatusForbidden, fmt.Sprintf("csr asks for DNS name %q, which HostConfig does not list for this agent", d))
		}
	}
	for _, ip := range csr.IPAddresses {
		if !slices.ContainsFunc(id.IPAddresses, ip.Equal) {
			return refuse(http.StatusForbidden, fmt.Sprintf("csr asks for IP address %s, which HostConfig does not list for this agent", ip))
		}
	}
	return nil
}

// consume marks the token used. The update is conditional on the
// resourceVersion read in checkToken: when another request used (or the
// reconciler replaced) the token in between, it fails and the request
// is refused.
func (h *EnrollHandler) consume(ctx context.Context, s *corev1.Secret) error {
	markEnrolled(s, h.now())
	err := h.Client.Update(ctx, s)
	if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return &enrollError{status: http.StatusUnauthorized, msg: errTokenRefused, cause: errTokenUsed}
	}
	if err != nil {
		return internalError("cannot record the enrollment", err)
	}
	return nil
}

func (h *EnrollHandler) now() time.Time {
	if h.Clock == nil {
		return SystemClock.Now()
	}
	return h.Clock.Now()
}

func (h *EnrollHandler) log() *slog.Logger {
	if h.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Log
}

func writeEnrollJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
