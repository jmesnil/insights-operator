./deploy-scanner.sh

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -a -o ./bin/insights-operator -ldflags="${GO_LDFLAGS}" ./cmd/insights-operator/main.go

podman build --platform linux/amd64 -t quay.io/jmesnil/insight-operator-with-container-scanner -f ./Dockerfile-container-scanner .
podman push quay.io/jmesnil/insight-operator-with-container-scanner

oc apply -f ./manifests/06a-deployment-with-container-scanner.yaml -n openshift-insights