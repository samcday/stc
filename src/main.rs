use std::sync::Arc;
use std::time::Duration;
use futures::StreamExt;
use kube::{Client, Config};
use kube::config::KubeConfigOptions;
use kube::runtime::{Controller, watcher};
use kube::runtime::controller::Action;
use thiserror::Error;
use tracing::{error, info};
use stc::{SyncthingCluster, SyncthingClusterSpec};

#[derive(Debug, Error)]
enum Error {

}

async fn reconcile_syncthing_cluster(cluster: Arc<SyncthingCluster>, ctx: Arc<()>) -> Result<Action, Error> {
    info!("rad: {}", cluster.spec.storage_class_name);
    Ok(Action::await_change())
}

fn error_policy(_object: Arc<SyncthingCluster>, _error: &Error, _ctx: Arc<()>) -> Action {
    Action::requeue(Duration::from_secs(1))
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt::init();

    let client = Client::try_default().await?;

    let clusters = kube::Api::<SyncthingCluster>::all(client.clone());

    Controller::new(clusters, watcher::Config::default())
        .shutdown_on_signal()
        .run(reconcile_syncthing_cluster, error_policy, Arc::new(()))
        .for_each(|res| async move {
            match res {
                Ok(o) => info!("reconciled {:?}", o),
                Err(e) => error!("reconciliation error: {:?}", e),
            }
        })
        .await;

    Ok(())
}
