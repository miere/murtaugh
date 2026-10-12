//! Posts a chart into the session's own Slack thread, as Slack's `data_visualization` block: a
//! line, bar, area or pie chart that Slack draws itself.
//!
//! The agent sends categories and one list of values per series, in category order. Slack wants
//! every point labelled with its category and refuses a chart where they disagree, so the labels
//! are written here and cannot. Its limits are checked first, and a chart with too much data is
//! turned down as that, so the agent can aggregate or split it.
//!
//! Like every tool here it posts nowhere but the session's thread. When a reply is being written
//! there, the chart is placed where the reply has got to.

use async_trait::async_trait;
use murtaugh_slack::{Block, SlackClient};
use rax::tool::{ToolDef, ToolKind};
use serde_json::{Value, json};

use super::replying::Replying;
use super::{Context, Tool};
use crate::render;

pub const NAME: &str = "show_chart";

const MAX_SERIES: usize = 12;
const MAX_SEGMENTS: usize = 12;
const MAX_POINTS: usize = 20;
const MAX_LABEL: usize = 20;
const MAX_TITLE: usize = 50;

pub struct ShowChart {
    slack: Option<SlackClient>,
    replying: Replying,
}

impl ShowChart {
    pub fn new(slack: Option<SlackClient>, replying: Replying) -> Self {
        Self { slack, replying }
    }
}

#[async_trait]
impl Tool for ShowChart {
    fn def(&self) -> ToolDef {
        ToolDef {
            name: NAME.to_owned(),
            description: format!(
                "Show a line, bar, area or pie chart in this conversation's Slack thread. It \
                 appears where your reply has got to, so introduce it first and carry on after. \
                 One chart holds at most {MAX_SERIES} series of {MAX_POINTS} points each, and a \
                 pie at most {MAX_SEGMENTS} segments; for more, aggregate the data or show \
                 several charts. Labels and series names are at most {MAX_LABEL} characters."
            ),
            input_schema: Some(json!({
                "type": "object",
                "required": ["title", "kind", "categories", "series"],
                "properties": {
                    "title": {
                        "type": "string",
                        "description": "Shown above the chart. At most 50 characters.",
                    },
                    "kind": {"type": "string", "enum": ["line", "bar", "area", "pie"]},
                    "categories": {
                        "type": "array",
                        "items": {"type": "string"},
                        "description": "The labels along the x axis, left to right; for a pie, \
                            the name of each segment.",
                    },
                    "series": {
                        "type": "array",
                        "items": {
                            "type": "object",
                            "required": ["name", "values"],
                            "properties": {
                                "name": {"type": "string"},
                                "values": {
                                    "type": "array",
                                    "items": {"type": "number"},
                                    "description": "One value per category, in the same order.",
                                },
                            },
                        },
                        "description": "What is plotted. A pie takes exactly one series, whose \
                            values are all above zero.",
                    },
                    "x_label": {"type": "string", "description": "Names the x axis. Not for a pie."},
                    "y_label": {"type": "string", "description": "Names the y axis. Not for a pie."},
                },
            })),
            kind: ToolKind::Edit,
        }
    }

    async fn invoke(&self, _arguments: Value) -> Result<String, String> {
        Err("This chart has no conversation to be shown in.".into())
    }

    async fn invoke_in(&self, context: &Context, arguments: Value) -> Result<String, String> {
        let Some(slack) = &self.slack else {
            return Err("This gateway has no Slack credentials, so it cannot post.".into());
        };
        let Some(conversation) = &context.conversation else {
            return Err("This session has no Slack thread to post to.".into());
        };
        let chart = Chart::parse(&arguments).map_err(|why| format!("{why} Nothing was posted."))?;
        self.replying
            .place(slack, conversation, chart.fallback(), vec![chart.block()])
            .await
            .map_err(|err| format!("Slack did not take the chart: {err}. Nothing was posted."))?;
        Ok(format!("Showed the {} chart.", chart.kind.as_str()))
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Kind {
    Line,
    Bar,
    Area,
    Pie,
}

impl Kind {
    fn as_str(self) -> &'static str {
        match self {
            Kind::Line => "line",
            Kind::Bar => "bar",
            Kind::Area => "area",
            Kind::Pie => "pie",
        }
    }
}

#[derive(Debug)]
struct Series {
    name: String,
    values: Vec<Value>,
}

#[derive(Debug)]
struct Chart {
    title: String,
    kind: Kind,
    categories: Vec<String>,
    series: Vec<Series>,
    x_label: Option<String>,
    y_label: Option<String>,
}

fn words<'a>(arguments: &'a Value, name: &str) -> Option<&'a str> {
    arguments
        .get(name)
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|text| !text.is_empty())
}

/// Holds a label to what Slack allows, naming the one that is too long.
fn fits(what: &str, text: &str, max: usize) -> Result<(), String> {
    let chars = text.chars().count();
    match chars > max {
        true => Err(format!(
            "{what} is {chars} characters, and Slack allows {max}; shorten it."
        )),
        false => Ok(()),
    }
}

impl Chart {
    /// `Err` says what to change. When it is the amount of data, it says so and by how much.
    fn parse(arguments: &Value) -> Result<Chart, String> {
        let title = words(arguments, "title").ok_or("`title` is empty; say what the chart is.")?;
        fits("`title`", title, MAX_TITLE)?;
        let kind = match words(arguments, "kind") {
            Some("line") => Kind::Line,
            Some("bar") => Kind::Bar,
            Some("area") => Kind::Area,
            Some("pie") => Kind::Pie,
            _ => return Err("`kind` must be line, bar, area or pie.".into()),
        };
        let mut categories: Vec<String> = Vec::new();
        let given = arguments
            .get("categories")
            .and_then(Value::as_array)
            .ok_or("`categories` must be an array of labels.")?;
        for (index, label) in given.iter().enumerate() {
            let at = format!("`categories[{index}]`");
            let label = label.as_str().map(str::trim).unwrap_or_default();
            if label.is_empty() {
                return Err(format!("{at} must be a label, in words."));
            }
            fits(&at, label, MAX_LABEL)?;
            // Slack matches a point to its category by label, so two alike cannot be told apart.
            if categories.iter().any(|known| known == label) {
                return Err(format!(
                    "{at} repeats \"{label}\"; each category needs its own label."
                ));
            }
            categories.push(label.to_owned());
        }
        if categories.is_empty() {
            return Err("`categories` is empty; a chart needs at least one.".into());
        }
        let (most, each) = match kind {
            Kind::Pie => (MAX_SEGMENTS, "segments in one pie"),
            _ => (MAX_POINTS, "points in one series"),
        };
        if categories.len() > most {
            return Err(format!(
                "The chart has too much data: {} categories, and Slack draws at most {most} \
                 {each}. Aggregate them into fewer, or show several charts.",
                categories.len()
            ));
        }
        let given = arguments
            .get("series")
            .and_then(Value::as_array)
            .filter(|series| !series.is_empty())
            .ok_or("`series` must be an array with at least one series.")?;
        if kind == Kind::Pie && given.len() != 1 {
            return Err(format!(
                "A pie takes exactly one series, and `series` has {}. Use a bar chart to compare \
                 several, or show one pie for each.",
                given.len()
            ));
        }
        if given.len() > MAX_SERIES {
            return Err(format!(
                "The chart has too much data: {} series, and Slack draws at most {MAX_SERIES} in \
                 one chart. Show fewer, or split them across several charts.",
                given.len()
            ));
        }
        let mut series: Vec<Series> = Vec::with_capacity(given.len());
        for (index, one) in given.iter().enumerate() {
            let at = format!("`series[{index}]`");
            let name = words(one, "name").ok_or_else(|| format!("{at} needs a `name`."))?;
            fits(&format!("{at}.name"), name, MAX_LABEL)?;
            if series.iter().any(|known| known.name == name) {
                return Err(format!(
                    "{at} repeats the name \"{name}\"; each series needs its own."
                ));
            }
            let values = one
                .get("values")
                .and_then(Value::as_array)
                .ok_or_else(|| format!("{at}.values must be an array of numbers."))?;
            if values.len() != categories.len() {
                return Err(format!(
                    "{at} has {} values and the chart has {} categories; every series needs one \
                     value per category, in the same order.",
                    values.len(),
                    categories.len()
                ));
            }
            for (v, value) in values.iter().enumerate() {
                let Some(number) = value.as_f64() else {
                    return Err(format!("{at}.values[{v}] must be a number."));
                };
                if kind == Kind::Pie && number <= 0.0 {
                    return Err(format!(
                        "{at}.values[{v}] is {value}; every segment of a pie must be above zero."
                    ));
                }
            }
            series.push(Series {
                name: name.to_owned(),
                values: values.clone(),
            });
        }
        let axis = |name: &str| -> Result<Option<String>, String> {
            let Some(label) = words(arguments, name) else {
                return Ok(None);
            };
            if kind == Kind::Pie {
                return Err(format!(
                    "`{name}` names an axis, and a pie has none; leave it out."
                ));
            }
            fits(&format!("`{name}`"), label, MAX_TITLE)?;
            Ok(Some(label.to_owned()))
        };
        Ok(Chart {
            title: title.to_owned(),
            kind,
            x_label: axis("x_label")?,
            y_label: axis("y_label")?,
            categories,
            series,
        })
    }

    fn points(&self, series: &Series) -> Vec<Value> {
        let labelled = self.categories.iter().zip(&series.values);
        labelled
            .map(|(label, value)| json!({"label": label, "value": value}))
            .collect()
    }

    fn block(&self) -> Block {
        let chart = match self.kind {
            Kind::Pie => {
                let segments = self.series.first().map(|series| self.points(series));
                json!({"type": "pie", "segments": segments.unwrap_or_default()})
            }
            kind => {
                let series: Vec<Value> = self
                    .series
                    .iter()
                    .map(|series| json!({"name": series.name, "data": self.points(series)}))
                    .collect();
                let mut axis = json!({"categories": self.categories});
                for (key, label) in [("x_label", &self.x_label), ("y_label", &self.y_label)] {
                    if let Some(label) = label {
                        axis[key] = json!(label);
                    }
                }
                json!({"type": kind.as_str(), "series": series, "axis_config": axis})
            }
        };
        Block::Raw(json!({"type": "data_visualization", "title": self.title, "chart": chart}))
    }

    /// The chart in words, for a notification and for anything that reads the thread back.
    fn fallback(&self) -> String {
        let mut out = format!(
            "*{}* ({} chart)",
            render::escape(&self.title),
            self.kind.as_str()
        );
        for series in &self.series {
            let points: Vec<String> = self
                .categories
                .iter()
                .zip(&series.values)
                .map(|(label, value)| format!("{label} {value}"))
                .collect();
            let line = format!("{}: {}", series.name, points.join(", "));
            out.push('\n');
            out.push_str(&render::escape(&line));
        }
        out
    }
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::expect_used)]

    use super::*;

    fn sales(kind: &str) -> Value {
        json!({
            "title": "Weekly sales",
            "kind": kind,
            "categories": ["W1", "W2"],
            "series": [
                {"name": "Online", "values": [120, 135.5]},
                {"name": "Retail", "values": [80, -5]},
            ],
            "x_label": "Week",
        })
    }

    /// Slack refuses a point whose label is not a category, so each one is labelled here.
    #[test]
    fn values_in_category_order_become_labelled_points() {
        for kind in ["line", "bar", "area"] {
            let chart = Chart::parse(&sales(kind)).unwrap();
            assert_eq!(
                chart.block().to_json(),
                json!({
                    "type": "data_visualization",
                    "title": "Weekly sales",
                    "chart": {
                        "type": kind,
                        "series": [
                            {"name": "Online", "data": [
                                {"label": "W1", "value": 120},
                                {"label": "W2", "value": 135.5},
                            ]},
                            {"name": "Retail", "data": [
                                {"label": "W1", "value": 80},
                                {"label": "W2", "value": -5},
                            ]},
                        ],
                        "axis_config": {"categories": ["W1", "W2"], "x_label": "Week"},
                    },
                })
            );
        }
    }

    #[test]
    fn a_pie_is_one_series_cut_into_segments() {
        let pie = json!({
            "title": "Share",
            "kind": "pie",
            "categories": ["Api", "Web"],
            "series": [{"name": "Requests", "values": [3, 1]}],
        });
        let chart = Chart::parse(&pie).unwrap();
        assert_eq!(
            chart.block().to_json(),
            json!({
                "type": "data_visualization",
                "title": "Share",
                "chart": {"type": "pie", "segments": [
                    {"label": "Api", "value": 3},
                    {"label": "Web", "value": 1},
                ]},
            })
        );
        assert_eq!(
            chart.fallback(),
            "*Share* (pie chart)\nRequests: Api 3, Web 1"
        );
    }

    /// The refusal Miere asked for: when there is too much data it says so, and by how much, so
    /// the agent aggregates or splits the chart instead of guessing what was wrong with it.
    #[test]
    fn too_much_data_is_refused_as_too_much_data() {
        let labels = |n: usize| (0..n).map(|i| format!("c{i}")).collect::<Vec<_>>();
        let zeros = |n: usize| vec![1; n];
        let points = json!({"title": "T", "kind": "line", "categories": labels(21),
            "series": [{"name": "A", "values": zeros(21)}]});
        let why = Chart::parse(&points).unwrap_err();
        assert!(why.contains("too much data: 21 categories"), "{why}");
        assert!(why.contains("at most 20 points"), "{why}");

        let segments = json!({"title": "T", "kind": "pie", "categories": labels(13),
            "series": [{"name": "A", "values": zeros(13)}]});
        let why = Chart::parse(&segments).unwrap_err();
        assert!(why.contains("too much data: 13 categories"), "{why}");
        assert!(why.contains("at most 12 segments"), "{why}");

        let series: Vec<Value> = (0..13)
            .map(|i| json!({"name": format!("s{i}"), "values": [1]}))
            .collect();
        let many = json!({"title": "T", "kind": "bar", "categories": ["A"], "series": series});
        let why = Chart::parse(&many).unwrap_err();
        assert!(why.contains("too much data: 13 series"), "{why}");
    }

    #[test]
    fn a_chart_slack_would_refuse_says_what_to_change() {
        let with = |change: Value| {
            let mut chart = sales("line");
            for (key, value) in change.as_object().unwrap() {
                chart[key] = value.clone();
            }
            Chart::parse(&chart).unwrap_err()
        };
        let one = |values: Value| json!([{"name": "A", "values": values}]);
        for (change, why) in [
            (json!({"title": ""}), "`title` is empty"),
            (json!({"title": "x".repeat(51)}), "`title` is 51 characters"),
            (json!({"kind": "scatter"}), "`kind` must be"),
            (json!({"categories": ["W1", "W1"]}), "repeats \"W1\""),
            (
                json!({"categories": ["W1", "x".repeat(21)]}),
                "`categories[1]` is 21 characters",
            ),
            (json!({"series": []}), "at least one series"),
            (
                json!({"series": one(json!([1]))}),
                "has 1 values and the chart has 2 categories",
            ),
            (
                json!({"series": one(json!([1, "2"]))}),
                "values[1] must be a number",
            ),
            (
                json!({"series": [{"name": "x".repeat(21), "values": [1, 2]}]}),
                "is 21 characters",
            ),
            (
                json!({"series": [{"name": "A", "values": [1, 2]}, {"name": "A", "values": [1, 2]}]}),
                "repeats the name",
            ),
            (json!({"kind": "pie"}), "exactly one series"),
            (
                json!({"kind": "pie", "series": one(json!([1, 0])), "x_label": null}),
                "above zero",
            ),
            (
                json!({"kind": "pie", "series": one(json!([1, 2]))}),
                "a pie has none",
            ),
        ] {
            let refused = with(change.clone());
            assert!(refused.contains(why), "{change}: {refused}");
        }
    }
}
