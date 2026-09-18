//! Files people drop into a conversation reach the agent as `gateway://files/<id>` links, and the
//! node fetches the bytes with RAX's `resource.read`. Only a link this gateway sent to that very
//! node is served, so a node cannot read files by guessing identifiers.

use std::collections::HashMap;
use std::sync::{Arc, Mutex, MutexGuard};

use murtaugh_slack::{FileRef, SlackClient};
use rax::content::ContentBlock;
use rax::id::RequestId;
use rax::resource::ReadResource;
use rax::{Error, ErrorKind};
use rax_tokio::gateway::GatewayLink;

pub const SCHEME: &str = "gateway";

#[derive(Clone, Default)]
pub struct Files {
    slack: Option<SlackClient>,
    /// Slack file ids by `"<node> <uri>"`: what each node was offered.
    served: Arc<Mutex<HashMap<String, String>>>,
}

pub fn uri(file_id: &str) -> String {
    format!("{SCHEME}://files/{file_id}")
}

impl Files {
    pub fn new(slack: SlackClient) -> Self {
        Self {
            slack: Some(slack),
            served: Arc::default(),
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
