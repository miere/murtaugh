//! A Slack client for the Murtaugh gateway: Socket Mode plus the few Web API methods it calls.
//! The Web API base URL is injectable so tests can point it at `slack-sim`.

mod blocks;
mod error;
mod events;
mod socket;
mod stream;
mod web;

pub use blocks::{Block, Button, ButtonStyle, Text, mrkdwn};
pub use error::SlackError;
pub use events::{Click, Event, EventEnvelope, FileRef, HomeClick, SocketEvent};
pub use socket::SocketMode;
pub use stream::{
    Chunk, Icon, PlanBlock, PlanTask, RichText, STREAM_FINALIZED, STREAMING_UNSUPPORTED,
    StartStream, TaskDisplayMode, TaskStatus,
};
pub use web::{
    DEFAULT_API_BASE, FileInfo, Identity, Message, PostMessage, Posted, SlackClient, Tokens,
    UpdateMessage, Upload,
};
