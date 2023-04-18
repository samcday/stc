#!/bin/bash
set -uexo pipefail
go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
controller-gen object paths="./..."
controller-gen crd paths="./..." output:dir=chart/crds
controller-gen rbac:roleName='"{{ include \"stc.fullname\" . }}"' paths=./... output:dir=chart/templates
