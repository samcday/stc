use kube::CustomResourceExt;
use stc::SyncthingCluster;

fn main() {
    println!("{}", serde_yaml::to_string(&SyncthingCluster::crd()).unwrap());
}
