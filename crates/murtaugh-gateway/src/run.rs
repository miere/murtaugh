//! `murtaugh-gateway run`: one Slack connection, one node endpoint, until a signal stops it.

use std::collections::{HashSet, VecDeque};
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use murtaugh_slack::{
    Click, HomeClick, SlackClient, SocketEvent, SocketMode, Tokens, ViewSubmission,
};
use murtaugh_store::{Leader, Lease};
use tokio::time::Instant;
use tokio_util::sync::CancellationToken;
use url::Url;

use crate::access::{Access, Snapshot};
use crate::approval;
use crate::chat::{self, Chat};
use crate::config;
use crate::files::Files;
use crate::fleet::Fleet;
use crate::hub::{self, REFRESH};

pub const SLACK_API: &str = "https://slack.com/api/";
const REMEMBERED_EVENTS: usize = 1024;

pub struct Options {
    pub slack_api: Url,
    /// How soon a CLI change to access reaches this gateway.
    pub refresh: Duration,
    /// How long a tool approval waits for the node's owner before denying.
    pub approval_timeout: Duration,
    /// How long a question or plan card waits for an answer.
    pub prompt_timeout: Duration,
    /// How long a turn may go without a word from the agent before it is stopped.
    pub turn_idle_timeout: Duration,
    /// How long one tool call may hold a turn open before it is taken for wedged.
    pub tool_ceiling: Duration,
}

impl Default for Options {
    fn default() -> Self {
        Self {
            slack_api: Url::parse(SLACK_API).unwrap_or_else(|_| unreachable!()),
            refresh: REFRESH,
            approval_timeout: approval::TIMEOUT,
            prompt_timeout: crate::prompts::TIMEOUT,
            turn_idle_timeout: chat::TURN_IDLE_TIMEOUT,
            tool_ceiling: chat::TOOL_CEILING,
        }
    }
}

pub async fn run(path: &Path) -> Result<String, String> {
    let config = config::load(path).map_err(|err| err.to_string())?;
    crate::logging::init(&config.log);
    println!("{}", banner(&config));
    let slack_api = Url::parse(SLACK_API).map_err(|err| err.to_string())?;
    let shutdown = CancellationToken::new();
    spawn_signals(shutdown.clone())?;
    let options = Options {
        slack_api,
        ..Options::default()
    };
    serve(path, options, shutdown).await?;
    Ok(String::new())
}

pub async fn serve(
    path: &Path,
    options: Options,
    shutdown: CancellationToken,
) -> Result<(), String> {
    let config = config::load(path).map_err(|err| err.to_string())?;
    let (store, leader) = crate::open_leader(&config.database).await?;
    let holder = format!(
        "{}-{}-{:08x}",
        hostname(),
        std::process::id(),
        rand::random::<u32>()
    );
    let slack = SlackClient::new(
        Tokens {
            app: config.slack.app_token.clone(),
            bot: config.slack.bot_token.clone(),
        },
        options.slack_api,
    );
    let identity = slack
        .auth_test()
        .await
        .map_err(|err| format!("Slack refused the bot token: {err}"))?;
    let reconciled = crate::roles::reconcile(&*store)
        .await
        .map_err(|err| err.to_string())?;
    if !reconciled.allowed.is_empty() || !reconciled.revoked.is_empty() {
        tracing::info!(
            allowed = ?reconciled.allowed,
            revoked = ?reconciled.revoked,
            "brought access into line: node admins are allowed, and tokens without a grant are revoked"
        );
    }
    let snapshot = Snapshot::load(&*store)
        .await
        .map_err(|err| err.to_string())?;
    if snapshot.admin().is_none() {
        tracing::warn!(
            "this gateway has no admin yet; run `murtaugh-gateway admin set <slack user id>`"
        );
    }
    let access = Access::reloading(snapshot, store.clone());
    let fleet = Fleet::default();
    let files = Files::new(slack.clone());
    let tools = crate::tools::Tools::new(vec![
        Box::new(crate::tools::slack_message::SlackReadMessage::new(Some(
            slack.clone(),
        ))),
        Box::new(crate::tools::canvas::read::ReadCanvas::new(Some(
            slack.clone(),
        ))),
        Box::new(crate::tools::canvas::edit::EditCanvas::new(Some(
            slack.clone(),
        ))),
    ]);
    let lent = crate::tools::Lent::new(store.clone());
    let sign_ins = crate::signin::SignIns::new(Some(slack.clone()), access.clone());
    let approvals = crate::approval::Approvals::default();
    let relay = crate::relay::Relay::new(
        access.clone(),
        fleet.clone(),
        store.clone(),
        lent.clone(),
        files.clone(),
        sign_ins.clone(),
        Some(crate::relay::OwnerApproval {
            slack: slack.clone(),
            approvals: approvals.clone(),
            timeout: options.approval_timeout,
        }),
    );
    let mut hub = hub::start(
        config.listen,
        hub::RETAIN_FOR,
        access.clone(),
        fleet.clone(),
        files.clone(),
        tools.clone(),
        lent.clone(),
        sign_ins.clone(),
        relay.clone(),
        shutdown.clone(),
    )
    .await
    .map_err(|err| format!("cannot listen for RAX on {}: {err}", config.listen))?;
    hub.server.stop_serving();
    tokio::spawn(hub::refresh(
        store.clone(),
        access.clone(),
        fleet.clone(),
        relay.clone(),
        options.refresh,
        shutdown.clone(),
    ));
    let chat = Chat::new(chat::Parts {
        slack: slack.clone(),
        bot_user: identity.user_id.clone(),
        team: identity.team_id.clone(),
        store: store.clone(),
        access,
        fleet,
        files,
        turn_timings: config.log.turn_timings,
        approval_timeout: options.approval_timeout,
        prompt_timeout: options.prompt_timeout,
        sign_ins,
        turn_idle_timeout: options.turn_idle_timeout,
        tool_ceiling: options.tool_ceiling,
        tools,
        lent,
        relay,
        approvals,
    });
    tracing::info!(listen = %hub.server.local_addr(), team = %identity.team_id, %holder, "murtaugh gateway started; waiting to lead");
    let mut seen = Seen::default();
    loop {
        let Some(lease) = lead(&*leader, &holder, &shutdown).await else {
            return Ok(());
        };
        tracing::info!(%holder, "leading: serving Slack and nodes");
        for pin in store.pins().await.map_err(|err| err.to_string())? {
            let _ = store.remove_pin(&pin.conversation).await;
        }
        hub.server.start_serving();
        let outcome = serve_as_leader(
            &*leader, lease, &slack, &chat, &mut hub, &mut seen, &shutdown,
        )
        .await;
        hub.server.stop_serving();
        match outcome {
            Led::Shutdown => {
                tracing::info!("murtaugh gateway stopped");
                return Ok(());
            }
            Led::Lost => tracing::warn!("lost the lead; standing by"),
            Led::SlackFailed(reason) => {
                tracing::error!(%reason, "the Slack connection failed; standing by before trying again");
                tokio::select! {
                    () = tokio::time::sleep(STANDBY_AFTER_FAILURE) => {}
                    () = shutdown.cancelled() => return Ok(()),
                }
            }
        }
    }
}

const STANDBY_AFTER_FAILURE: Duration = Duration::from_secs(5);

enum Led {
    Shutdown,
    Lost,
    SlackFailed(String),
}

async fn lead(leader: &dyn Leader, holder: &str, shutdown: &CancellationToken) -> Option<Lease> {
    let poll = leader.ttl() / 3;
    loop {
        match leader.acquire(holder).await {
            Ok(Some(lease)) => return Some(lease),
            Ok(None) => tracing::debug!(%holder, "another gateway leads; standing by"),
            Err(err) => tracing::warn!(error = %err, "could not contend for the lead"),
        }
        tokio::select! {
            () = tokio::time::sleep(poll) => {}
            () = shutdown.cancelled() => return None,
        }
    }
}

async fn serve_as_leader(
    leader: &dyn Leader,
    mut lease: Lease,
    slack: &SlackClient,
    chat: &Arc<Chat>,
    hub: &mut hub::Hub,
    seen: &mut Seen,
    shutdown: &CancellationToken,
) -> Led {
    let ttl = leader.ttl();
    let mut renewed_at = Instant::now();
    let mut socket = match SocketMode::connect(slack.clone()).await {
        Ok(socket) => socket,
        Err(err) => {
            let _ = leader.release(lease).await;
            return Led::SlackFailed(err.to_string());
        }
    };
    let mut renew = tokio::time::interval(ttl / 3);
    renew.tick().await;
    let (outcome, release) = loop {
        tokio::select! {
            event = socket.next() => match event {
                Some(SocketEvent::Event(envelope)) => {
                    if seen.first_time(&envelope.event_id) {
                        tokio::spawn(chat.clone().on_event(envelope.event));
                    }
                }
                Some(SocketEvent::SlashCommand(payload)) => {
                    tokio::spawn(chat.clone().on_slash(payload));
                }
                Some(SocketEvent::Interactive(payload)) => {
                    if let Some(click) = Click::from_interactive(&payload) {
                        tokio::spawn(chat.clone().on_click(click));
                    } else if let Some(click) = HomeClick::from_interactive(&payload) {
                        tokio::spawn(chat.clone().on_home_click(click));
                    } else if let Some(submission) = ViewSubmission::from_interactive(&payload) {
                        tokio::spawn(chat.clone().on_view_submission(submission));
                    }
                }
                None => break (Led::SlackFailed("the Slack connection ended".into()), true),
            },
            Some(change) = hub.changes.recv() => {
                tokio::spawn(chat.clone().on_fleet(change));
            }
            Some(background) = hub.backgrounds.recv() => chat.on_background(background),
            _ = renew.tick() => match leader.renew(&lease).await {
                Ok(Some(next)) => {
                    lease = next;
                    renewed_at = Instant::now();
                }
                Ok(None) => break (Led::Lost, false),
                Err(err) if renewed_at.elapsed() < ttl * 2 / 3 => {
                    tracing::warn!(error = %err, "could not renew the lead; trying again");
                }
                Err(err) => {
                    tracing::error!(error = %err, "could not renew the lead in time; stepping down");
                    break (Led::Lost, false);
                }
            },
            () = shutdown.cancelled() => break (Led::Shutdown, true),
        }
    };
    socket.close().await;
    if release {
        let _ = leader.release(lease).await;
    }
    outcome
}

pub fn banner(config: &config::Config) -> String {
    let database = match &config.database {
        config::Database::Sqlite { path } => format!("sqlite {}", path.display()),
        config::Database::Firestore(firestore) => format!(
            "firestore {}",
            firestore.collection.as_deref().unwrap_or("murtaugh")
        ),
    };
    format!(
        "murtaugh-gateway {}\nconfig: {}\nstore: {database}\nnodes and clients: ws://{}/rax/v1/link",
        crate::version::VERSION,
        config.path.display(),
        config.listen
    )
}

fn hostname() -> String {
    std::env::var("HOSTNAME")
        .ok()
        .filter(|name| !name.is_empty())
        .unwrap_or_else(|| "gateway".to_owned())
}

#[derive(Default)]
struct Seen {
    ids: HashSet<String>,
    order: VecDeque<String>,
}

impl Seen {
    fn first_time(&mut self, id: &str) -> bool {
        if !self.ids.insert(id.to_owned()) {
            return false;
        }
        self.order.push_back(id.to_owned());
        if self.order.len() > REMEMBERED_EVENTS
            && let Some(oldest) = self.order.pop_front()
        {
            self.ids.remove(&oldest);
        }
        true
    }
}

fn spawn_signals(shutdown: CancellationToken) -> Result<(), String> {
    use tokio::signal::unix::{SignalKind, signal};
    let mut terminate =
        signal(SignalKind::terminate()).map_err(|err| format!("cannot watch SIGTERM: {err}"))?;
    let mut interrupt =
        signal(SignalKind::interrupt()).map_err(|err| format!("cannot watch SIGINT: {err}"))?;
    tokio::spawn(async move {
        tokio::select! {
            _ = terminate.recv() => {}
            _ = interrupt.recv() => {}
        }
        shutdown.cancel();
    });
    Ok(())
}
