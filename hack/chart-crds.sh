#!/bin/sh
# Copies the CRDs controller-gen writes to config/crd/bases into the Helm chart
# as regular templates (DESIGN §11: helm upgrade updates them, helm uninstall
# removes them unless crds.keep). Run by `make manifests`, so the chart never
# drifts from the API types; CI's generated-code check covers the copies.
set -eu

src=config/crd/bases
dst=charts/hvmc/templates/crds

mkdir -p "$dst"
rm -f "$dst"/*.yaml

for f in "$src"/*.yaml; do
	awk -v src="$f" '
		index($0, "{{") {
			printf "%s:%d: \"{{\" would be read as a Helm template\n", src, NR > "/dev/stderr"
			failed = 1
			exit 1
		}
		NR == 1 { print "{{- /* Generated from " src " by make manifests. DO NOT EDIT. */}}" }
		/^  name: / && !labels {
			print "  labels:"
			print "    {{- include \"hvmc.labels\" . | nindent 4 }}"
			labels = 1
		}
		{ print }
		/^  annotations:$/ && !keep {
			print "    {{- if .Values.crds.keep }}"
			print "    helm.sh/resource-policy: keep"
			print "    {{- end }}"
			keep = 1
		}
		END {
			if (failed) exit 1
			if (!labels || !keep) {
				printf "%s: metadata.annotations or metadata.name not found\n", src > "/dev/stderr"
				exit 1
			}
		}
	' "$f" >"$dst/$(basename "$f")"
done
