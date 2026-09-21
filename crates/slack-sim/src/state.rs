use std::collections::{BTreeMap, HashMap, HashSet, VecDeque};
use std::time::{SystemTime, UNIX_EPOCH};

use serde_json::Value;
use tokio::sync::mpsc::UnboundedSender;

use crate::{
    ALICE, BOB, BOT_USER_ID, Call, ChannelKind, GENERAL, PRIVATE, RANDOM, Reaction, SECRET,
    SimEphemeral, SimMessage, Violation,
};

pub(crate) const APP_TOKEN: &str = "xapp-1-A0SIM0001-1-slacksimapptoken";
pub(crate) const BOT_TOKEN: &str = "xoxb-1-1-slacksimbottoken";
pub(crate) const VERIFICATION_TOKEN: &str = "slacksimverificationtoken";

pub(crate) struct User {
    pub name: String,
}

pub(crate) struct Channel {
    pub id: String,
    pub name: String,
    pub kind: ChannelKind,
    pub bot_member: bool,
    pub messages: Vec<SimMessage>,
}

impl Channel {
    pub fn find(&self, ts: &str) -> Option<&SimMessage> {
        self.messages.iter().find(|m| m.ts == ts)
    }

    pub fn find_mut(&mut self, ts: &str) -> Option<&mut SimMessage> {
        self.messages.iter_mut().find(|m| m.ts == ts)
    }

    pub fn thread_root(&self, ts: &str) -> Option<String> {
        let msg = self.find(ts)?;
        Some(msg.thread_ts.clone().unwrap_or_else(|| msg.ts.clone()))
    }

    pub fn thread(&self, root: &str) -> Vec<SimMessage> {
        self.messages
            .iter()
            .filter(|m| m.ts == root || m.thread_ts.as_deref() == Some(root))
            .cloned()
            .collect()
    }

    pub fn replies(&self, root: &str) -> Vec<&SimMessage> {
        self.messages
            .iter()
            .filter(|m| m.ts != root && m.thread_ts.as_deref() == Some(root))
            .collect()
    }

    pub fn event_channel_type(&self) -> &'static str {
        match self.kind {
            ChannelKind::Public => "channel",
            ChannelKind::Private => "group",
            ChannelKind::Im => "im",
        }
    }
}

pub(crate) struct File {
    pub id: String,
    pub name: String,
    pub title: Option<String>,
    pub mimetype: String,
    pub bytes: Vec<u8>,
    pub user: String,
    pub created: u64,
    pub channel: String,
}

pub(crate) enum Fault {
    RateLimit(u64),
    Fail(String),
}

pub(crate) enum Out {
    Text(String),
    Close,
    Drop,
}

pub(crate) struct Conn {
    pub id: u64,
    pub tx: UnboundedSender<Out>,
    pub disconnect_sent: bool,
}

pub(crate) struct Pending {
    pub envelope: Value,
    pub attempt: u64,
}

pub(crate) struct State {
    pub base: String,
    pub users: BTreeMap<String, User>,
    pub channels: BTreeMap<String, Channel>,
    pub files: BTreeMap<String, File>,
    pub calls: Vec<Call>,
    pub violations: Vec<Violation>,
    pub faults: HashMap<String, VecDeque<Fault>>,
    pub tickets: HashSet<String>,
    pub conns: Vec<Conn>,
    pub pending: HashMap<String, Pending>,
    pub acked: Vec<String>,
    pub undelivered: VecDeque<Value>,
    pub replies_page_size: usize,
    pub channel_message_events: bool,
    pub thread_statuses: HashMap<(String, String), String>,
    pub reserved: HashMap<String, crate::upload::Reserved>,
    pub ephemerals: Vec<SimEphemeral>,
    pub homes: HashMap<String, Value>,
    clock: u64,
    seq: u64,
}

impl State {
    pub fn new(base: String) -> State {
        let mut state = State {
            base,
            users: BTreeMap::new(),
            channels: BTreeMap::new(),
            files: BTreeMap::new(),
            calls: Vec::new(),
            violations: Vec::new(),
            faults: HashMap::new(),
            tickets: HashSet::new(),
            conns: Vec::new(),
            pending: HashMap::new(),
            acked: Vec::new(),
            undelivered: VecDeque::new(),
            replies_page_size: 1000,
            channel_message_events: true,
            thread_statuses: HashMap::new(),
            reserved: HashMap::new(),
            ephemerals: Vec::new(),
            homes: HashMap::new(),
            clock: 0,
            seq: 0,
        };
        state.add_user(BOT_USER_ID, "murtaugh");
        state.add_user(ALICE, "alice");
        state.add_user(BOB, "bob");
        state.add_channel(GENERAL, "general", ChannelKind::Public, true);
        state.add_channel(RANDOM, "random", ChannelKind::Public, false);
        state.add_channel(PRIVATE, "private", ChannelKind::Private, true);
        state.add_channel(SECRET, "secret", ChannelKind::Private, false);
        state
    }

    pub fn add_user(&mut self, id: &str, name: &str) {
        self.users.insert(
            id.to_owned(),
            User {
                name: name.to_owned(),
            },
        );
    }

    pub fn add_channel(&mut self, id: &str, name: &str, kind: ChannelKind, bot_member: bool) {
        self.channels.insert(
            id.to_owned(),
            Channel {
                id: id.to_owned(),
                name: name.to_owned(),
                kind,
                bot_member,
                messages: Vec::new(),
            },
        );
    }

    pub fn im_for(&mut self, user: &str) -> String {
        let id = format!("D0{}", user.trim_start_matches('U').trim_start_matches('0'));
        if !self.channels.contains_key(&id) {
            self.add_channel(&id, user, ChannelKind::Im, true);
        }
        id
    }

    pub fn next_ts(&mut self) -> String {
        self.clock = now_micros().max(self.clock + 1);
        format!("{}.{:06}", self.clock / 1_000_000, self.clock % 1_000_000)
    }

    pub fn next_seq(&mut self) -> u64 {
        self.seq += 1;
        self.seq
    }

    pub fn uuid(&mut self) -> String {
        let n = self.next_seq();
        format!(
            "5a1c0000-{:04x}-4{:03x}-8000-{:012x}",
            n >> 16,
            n & 0xfff,
            n
        )
    }

    pub fn violation(&mut self, method: &str, error: &str, detail: impl Into<String>) {
        self.violations.push(Violation {
            method: method.to_owned(),
            error: error.to_owned(),
            detail: detail.into(),
        });
    }

    pub fn file_url(&self, file: &File) -> String {
        format!(
            "{}files-pri/{}-{}/download/{}",
            self.base,
            crate::TEAM_ID,
            file.id,
            file.name
        )
    }

    pub fn post(
        &mut self,
        channel: &str,
        mut msg: SimMessage,
        thread_ts: Option<&str>,
    ) -> Result<SimMessage, &'static str> {
        let root = match thread_ts {
            Some(ts) => Some(
                self.channels
                    .get(channel)
                    .and_then(|c| c.thread_root(ts))
                    .ok_or("thread_not_found")?,
            ),
            None => None,
        };
        msg.ts = self.next_ts();
        msg.thread_ts = root.clone();
        let chan = self.channels.get_mut(channel).ok_or("channel_not_found")?;
        if let Some(root) = root
            && let Some(parent) = chan.find_mut(&root)
        {
            parent.thread_ts = Some(root);
        }
        chan.messages.push(msg.clone());
        Ok(msg)
    }
}

pub(crate) fn new_message(user: Option<&str>, text: &str) -> SimMessage {
    SimMessage {
        ts: String::new(),
        user: user.map(str::to_owned),
        bot_id: None,
        text: text.to_owned(),
        blocks: None,
        thread_ts: None,
        subtype: None,
        files: Vec::new(),
        reactions: Vec::<Reaction>::new(),
        edited: false,
        stream: None,
    }
}

pub(crate) fn now_secs() -> u64 {
    now_micros() / 1_000_000
}

fn now_micros() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_micros() as u64)
        .unwrap_or_default()
}
