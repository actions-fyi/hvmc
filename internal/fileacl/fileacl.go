// Package fileacl restricts who may read the agent's configuration, keys
// and state (DESIGN §4.3, §4.5).
//
// On Windows the permission bits Go passes to os.Chmod and os.MkdirAll set
// no ACL, so files in %ProgramData%\hvmc would inherit the ProgramData ACL,
// which lets every local user read them. [SecureDir] and [SecureConfig]
// replace the inherited ACL with a protected one:
//
//   - the state directory: SYSTEM and Administrators full control, the
//     service account modify, inherited by everything created in it (keys,
//     pending policy, audit log, logs);
//   - agent.yaml: SYSTEM and Administrators full control, the service
//     account read only, so the service cannot change its own policy.
//
// Elsewhere both functions do nothing: the callers' permission bits apply.
package fileacl

import (
	"crypto/sha1" //nolint:gosec // G505: the service SID is defined as the SHA-1 of the service name.
	"encoding/binary"
	"strconv"
	"strings"
	"unicode/utf16"
)

// ServiceName is the name of the agent's Windows service; its virtual
// account is NT SERVICE\<ServiceName>.
const ServiceName = "hvmc-agent"

// ServiceSID returns the SID of the virtual account NT SERVICE\<name>:
// S-1-5-80 followed by the SHA-1 of the upper-case service name in UTF-16LE
// as five little-endian sub-authorities. It is computed rather than looked
// up, so it is valid before the service is installed (`hvmc agent enroll`
// may run first).
func ServiceSID(name string) string {
	u := utf16.Encode([]rune(strings.ToUpper(name)))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	sum := sha1.Sum(b) //nolint:gosec // G401: see the import.
	var sb strings.Builder
	sb.WriteString("S-1-5-80")
	for i := 0; i < len(sum); i += 4 {
		sb.WriteByte('-')
		sb.WriteString(strconv.FormatUint(uint64(binary.LittleEndian.Uint32(sum[i:])), 10))
	}
	return sb.String()
}

// modifyAccess is the "Modify" right: read, write, execute and delete, but
// neither changing the ACL nor deleting other entries of a directory
// (FILE_DELETE_CHILD), so the service cannot replace agent.yaml.
const modifyAccess = "0x1301bf"

// dirSDDL is the protected DACL of the state directory.
func dirSDDL(service string) string {
	return "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;" + modifyAccess + ";;;" + service + ")"
}

// configSDDL is the protected DACL of agent.yaml.
func configSDDL(service string) string {
	return "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;" + service + ")"
}
