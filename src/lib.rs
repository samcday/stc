use kube::CustomResource;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

#[derive(Clone, CustomResource, Debug, Default, Deserialize, Eq, JsonSchema, PartialEq, Serialize)]
#[kube(
    group = "stc.samcday.com",
    version = "v1alpha1",
    kind = "SyncthingCluster",
    namespaced
)]
#[serde(rename_all = "camelCase")]
pub struct SyncthingClusterSpec {
    pub storage_class_name: String,
}
