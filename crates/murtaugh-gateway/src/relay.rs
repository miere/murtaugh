//! The RAX API, which is `rax-gateway`'s relay asking node owners in Slack.

use std::sync::Arc;

use murtaugh_store::Store;

use crate::access::Access;
use crate::files::Files;
use crate::fleet::Fleet;
use crate::owner::{OwnerApproval, SlackOwner};
use crate::signin::SignIns;
use crate::tools::Lent;

pub use rax_gateway::relay::{Relay, SCHEME, Summary};

pub fn new(
    access: Access,
    fleet: Fleet,
    store: Arc<dyn Store>,
    lent: Lent,
    files: Files,
    sign_ins: SignIns,
    owner_approval: Option<OwnerApproval>,
) -> Relay {
    let asking = owner_approval.map(|asking| (asking, store.clone()));
    Relay::new(
        access,
        fleet,
        store,
        crate::settings().clone(),
        lent.gateway(),
        files.transfers(),
        Arc::new(SlackOwner::new(sign_ins, asking)),
    )
}
