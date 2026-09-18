//! Files both ways. People's files reach the agent as `gateway://files/<id>` links, and the node
//! fetches the bytes with RAX's `resource.read`; only a link this gateway sent to that very node is
//! served, so a node cannot read files by guessing identifiers. The agent's attachments arrive as
//! transfer bytes on the link and, separately, as an `attachment` event on the turn.

use std::collections::HashMap;
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;

use murtaugh_slack::{FileRef, SlackClient};
use rax::content::ContentBlock;
use rax::id::{RequestId, TransferId};
use rax::resource::ReadResource;
use rax::{Error, ErrorKind};
use rax_tokio::gateway::GatewayLink;
use tokio::sync::oneshot;
use tokio::time::Instant;

pub const SCHEME: &str = "gateway";

#[derive(Clone, Default)]
pub struct Files {
    slack: Option<SlackClient>,
    /// Slack file ids by `"<node> <uri>"`: what each node was offered.
    served: Arc<Mutex<HashMap<String, String>>>,
    transfers: Arc<Mutex<HashMap<(String, TransferId), Transfer>>>,
}

pub type Delivered = Result<Vec<u8>, String>;

/// Bytes whose event has not come yet, or an event waiting for its bytes.
enum Transfer {
    Arrived(Delivered, Instant),
    Awaited(oneshot::Sender<Delivered>),
}

/// Bytes nobody claims within this are dropped, so a node cannot pile up memory.
const UNCLAIMED: Duration = Duration::from_secs(600);

pub fn uri(file_id: &str) -> String {
    format!("{SCHEME}://files/{file_id}")
}

impl Files {
    pub fn new(slack: SlackClient) -> Self {
        Self {
            slack: Some(slack),
            served: Arc::default(),
            transfers: Arc::default(),
        }
    }

    fn transfers(&self) -> MutexGuard<'_, HashMap<(String, TransferId), Transfer>> {
        self.transfers
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// The link finished (or failed) a transfer from `node`.
    pub fn arrived(&self, node: &str, transfer_id: TransferId, delivered: Delivered) {
        let mut transfers = self.transfers();
        transfers.retain(|_, t| !matches!(t, Transfer::Arrived(_, at) if at.elapsed() > UNCLAIMED));
        let key = (node.to_owned(), transfer_id);
        match transfers.remove(&key) {
            Some(Transfer::Awaited(waiter)) => {
                let _ = waiter.send(delivered);
            }
            _ => {
                transfers.insert(key, Transfer::Arrived(delivered, Instant::now()));
            }
        }
    }

    /// The bytes an `attachment` event names, waiting up to `wait` for them to arrive.
    pub async fn claim(&self, node: &str, transfer_id: &TransferId, wait: Duration) -> Delivered {
        let key = (node.to_owned(), transfer_id.clone());
        let waiting = {
            let mut transfers = self.transfers();
            match transfers.remove(&key) {
                Some(Transfer::Arrived(delivered, _)) => return delivered,
                _ => {
                    let (waiter, waiting) = oneshot::channel();
                    transfers.insert(key.clone(), Transfer::Awaited(waiter));
                    waiting
                }
            }
        };
        match tokio::time::timeout(wait, waiting).await {
            Ok(Ok(delivered)) => delivered,
            _ => {
                self.transfers().remove(&key);
                Err(format!(
                    "its bytes did not arrive within {}s",
                    wait.as_secs()
                ))
            }
        }
    }

    fn served(&self) -> MutexGuard<'_, HashMap<String, String>> {
        self.served
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// The link to put in a prompt for `node`; from now on that node, and only it, may read it.
    pub fn offer(&self, node: &str, file: &FileRef) -> ContentBlock {
        let link = uri(&file.id);
        self.served()
            .insert(format!("{node} {link}"), file.id.clone());
        ContentBlock::ResourceLink {
            uri: link,
            name: file.name.clone().unwrap_or_else(|| file.id.clone()),
            mime_type: file.mimetype.clone(),
            title: None,
            description: None,
            size: file.size,
        }
    }

    pub async fn serve(&self, link: &GatewayLink, node: &str, id: RequestId, read: ReadResource) {
        let file_id = self.served().get(&format!("{node} {}", read.uri)).cloned();
        let result = match (file_id, &self.slack) {
            (None, _) => Err(Error::new(
                ErrorKind::Forbidden,
                format!("this gateway never sent {} to this node", read.uri),
            )),
            (Some(_), None) => Err(Error::new(
                ErrorKind::NotFound,
                "this gateway cannot reach Slack",
            )),
            (Some(file_id), Some(slack)) => fetch(slack, &file_id, &read).await,
        };
        let sent = match result {
            Ok((bytes, mimetype)) => link
                .serve_resource(id, &read, &bytes, mimetype)
                .await
                .map(drop)
                .map_err(|err| err.to_string()),
            Err(error) => link.fault(id, error).await.map_err(|err| err.to_string()),
        };
        if let Err(err) = sent {
            tracing::warn!(uri = %read.uri, error = %err, "could not answer a node's file read");
        }
    }
}

async fn fetch(
    slack: &SlackClient,
    file_id: &str,
    read: &ReadResource,
) -> Result<(Vec<u8>, Option<String>), Error> {
    let info = slack.file_info(file_id).await.map_err(|err| {
        Error::new(
            ErrorKind::NotFound,
            format!("Slack could not find the file: {err}"),
        )
    })?;
    if let Some(max) = read.max_bytes
        && info.size > max
    {
        return Err(Error::new(
            ErrorKind::TooLarge,
            format!(
                "{} is {} bytes; the node takes at most {max}",
                info.name, info.size
            ),
        ));
    }
    let bytes = slack.download(&info).await.map_err(|err| {
        Error::new(
            ErrorKind::NotFound,
            format!("Slack would not hand over the file: {err}"),
        )
    })?;
    let mimetype = (!info.mimetype.is_empty()).then(|| info.mimetype.clone());
    Ok((bytes, mimetype))
}
