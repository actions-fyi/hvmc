// Package chart holds the golden tests of the Helm chart in charts/hvmc
// (DESIGN §11): `helm template` output and NOTES.txt for a set of values
// files, compared with testdata/. Helm is the version pinned in tools/go.mod.
//
// Regenerate the golden files with `make helm-golden`.
package chart
