//! Which threads the gateway is writing an agent's reply into, and the way a tool gets a message
//! of its own into one of them.
//!
//! Slack orders a thread by when each message started, so a message posted while a reply is being
//! written would sit below words the agent has yet to say. A tool hands its message to whoever is
//! writing the reply instead, and it is placed where the reply has got to.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use murtaugh_slack::{Block, PostMessage, Posted, SlackClient, SlackError};
use murtaugh_store::Conversation;
use tokio::sync::{mpsc, oneshot};

use super::lock;
use crate::reply::Reply;

/// The threads the gateway is writing an agent's reply into right now.
#[derive(Clone, Default)]
pub struct Replying {
    threads: Arc<Mutex<HashMap<Conversation, usize>>>,
    writers: Arc<Mutex<HashMap<Conversation, Vec<mpsc::UnboundedSender<Placement>>>>>,
}

impl Replying {
    /// Marks a thread until the guard is dropped. Counted, because a turn and a session's
    /// background run can both be writing into one thread.
    pub fn hold(&self, conversation: &Conversation) -> Held {
        *lock(&self.threads).entry(conversation.clone()).or_default() += 1;
        Held {
            replying: self.clone(),
            conversation: conversation.clone(),
        }
    }

    pub(super) fn contains(&self, conversation: &Conversation) -> bool {
        lock(&self.threads).contains_key(conversation)
    }

    /// Takes the messages tools post into a thread for as long as the receiver lives, so whoever
    /// is writing the reply there can place each one.
    pub fn writer(&self, conversation: &Conversation) -> mpsc::UnboundedReceiver<Placement> {
        let (sender, receiver) = mpsc::unbounded_channel();
        let mut writers = lock(&self.writers);
        writers.retain(|_, senders| {
            senders.retain(|sender| !sender.is_closed());
            !senders.is_empty()
        });
        writers
            .entry(conversation.clone())
            .or_default()
            .push(sender);
        receiver
    }

    /// Posts a message into a thread: through the reply being written there, or directly when
    /// there is none.
    pub async fn place(
        &self,
        slack: &SlackClient,
        conversation: &Conversation,
        text: String,
        blocks: Vec<Block>,
    ) -> Result<Posted, SlackError> {
        // The newest writer is the one whose words are going into the thread now.
        let writer = lock(&self.writers).get(conversation).and_then(|senders| {
            let live = senders.iter().rev().find(|sender| !sender.is_closed());
            live.cloned()
        });
        if let Some(writer) = writer {
            let (done, placed) = oneshot::channel();
            let placement = Placement {
                text: text.clone(),
                blocks: blocks.clone(),
                done,
            };
            // A reply that ended before it got to this one never posted it.
            if writer.send(placement).is_ok()
                && let Ok(result) = placed.await
            {
                return result;
            }
        }
        let message = PostMessage {
            channel: conversation.channel.clone(),
            thread_ts: Some(conversation.thread_ts.clone()),
            text,
            blocks,
        };
        slack.post_message(&message).await
    }
}

pub struct Held {
    replying: Replying,
    conversation: Conversation,
}

impl Drop for Held {
    fn drop(&mut self) {
        let mut threads = lock(&self.replying.threads);
        if let Some(held) = threads.get_mut(&self.conversation) {
            *held -= 1;
            if *held == 0 {
                threads.remove(&self.conversation);
            }
        }
    }
}

/// A message a tool wants in a thread whose reply is being written.
pub struct Placement {
    text: String,
    blocks: Vec<Block>,
    done: oneshot::Sender<Result<Posted, SlackError>>,
}

impl Placement {
    /// Places the message in the reply, and tells the tool what Slack did with it.
    pub async fn onto(self, reply: &mut Reply) {
        let result = reply.place(self.text, self.blocks).await;
        // A tool that stopped waiting has no use for the answer.
        let _ = self.done.send(result);
    }
}
