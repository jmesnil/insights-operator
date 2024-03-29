kubectl apply -f ~/tmp/container-scanner-pull-secret.yaml -n openshift-insights

kubectl apply -f ./manifests/010-clusterrole-container-scanner.yaml -n openshift-insights
kubectl apply -f ./manifests/010-container-scanner.yaml -n openshift-insights
oc adm policy add-scc-to-user -z container-scanner-sa -n openshift-insights privileged

kubectl wait --namespace openshift-insights \
  --for=condition=ready pod \
  --selector=app.kubernetes.io/name=container-scanner \
  --timeout=90s 

if [ $? -ne 0 ]; then
  echo "❌ Container scanner is not deployed in openshift-insights"
  exit 1
else
  echo "=========================================================================="
  echo "✅ Container scanner is deployed in openshift-insights"
  echo "=========================================================================="
fi
