# syncthing-controller (stc)

Create a `SyncthingCluster` in your Kubernetes:

```sh
make install
make run

kubectl apply -f config/samples/
kubectl wait syncthingcluster syncthingcluster-sample --for=condition=Ready
kubectl get -o yaml syncthingcluster syncthingcluster-sample
```
