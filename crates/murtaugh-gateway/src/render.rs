//! Agents write Markdown; Slack's `text` field reads mrkdwn. This converts the common subset and
//! splits long replies, since Slack rejects a message over 40,000 characters.

pub const MESSAGE_LIMIT: usize = 39_000;

pub fn mrkdwn(markdown: &str) -> String {
    let mut out = String::with_capacity(markdown.len());
    let mut fenced = false;
    for (index, line) in markdown.lines().enumerate() {
        if index > 0 {
            out.push('\n');
        }
        if line.trim_start().starts_with("```") {
            fenced = !fenced;
            out.push_str("```");
            continue;
        }
        if fenced {
            out.push_str(&escape(line));
            continue;
        }
        let trimmed = line.trim_start();
        let heading = trimmed.trim_start_matches('#');
        if heading.len() < trimmed.len() && heading.starts_with(' ') {
            out.push('*');
            out.push_str(&inline(heading.trim()));
            out.push('*');
            continue;
        }
        if let Some(item) = trimmed
            .strip_prefix("- ")
            .or_else(|| trimmed.strip_prefix("* "))
        {
            let indent = &line[..line.len() - trimmed.len()];
            out.push_str(indent);
            out.push_str("• ");
            out.push_str(&inline(item));
            continue;
        }
        out.push_str(&inline(line));
    }
    if markdown.ends_with('\n') {
        out.push('\n');
    }
    out
}

pub fn escape(text: &str) -> String {
    text.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
}

fn inline(line: &str) -> String {
    let mut out = String::with_capacity(line.len());
    let mut rest = line;
    while !rest.is_empty() {
        if let Some(after) = rest.strip_prefix('`')
            && let Some(end) = after.find('`')
        {
            out.push('`');
            out.push_str(&escape(&after[..end]));
            out.push('`');
            rest = &after[end + 1..];
            continue;
        }
        if let Some(after) = rest.strip_prefix("**")
            && let Some(end) = after.find("**")
        {
            out.push('*');
            out.push_str(&inline(&after[..end]));
            out.push('*');
            rest = &after[end + 2..];
            continue;
        }
        if let Some(after) = rest.strip_prefix("~~")
            && let Some(end) = after.find("~~")
        {
            out.push('~');
            out.push_str(&inline(&after[..end]));
            out.push('~');
            rest = &after[end + 2..];
            continue;
        }
        if let Some(after) = rest.strip_prefix('[')
            && let Some(close) = after.find("](")
            && let Some(end) = after[close + 2..].find(')')
        {
            let label = &after[..close];
            let url = &after[close + 2..close + 2 + end];
            if url.starts_with("http://")
                || url.starts_with("https://")
                || url.starts_with("mailto:")
            {
                out.push('<');
                out.push_str(url);
                out.push('|');
                out.push_str(&escape(label).replace('|', "¦"));
                out.push('>');
                rest = &after[close + 2 + end + 1..];
                continue;
            }
        }
        let Some(next) = rest.chars().next() else {
            break;
        };
        out.push_str(&escape(&next.to_string()));
        rest = &rest[next.len_utf8()..];
    }
    out
}

/// Splits on line boundaries where it can, so a code block or list is rarely cut mid-line.
pub fn split(text: &str, limit: usize) -> Vec<String> {
    let mut parts = Vec::new();
    let mut rest = text;
    while rest.chars().count() > limit {
        let hard = rest
            .char_indices()
            .nth(limit)
            .map_or(rest.len(), |(index, _)| index);
        let cut = if rest[hard..].starts_with('\n') {
            hard
        } else {
            rest[..hard]
                .rfind('\n')
                .filter(|&at| at > 0)
                .unwrap_or(hard)
        };
        parts.push(rest[..cut].to_owned());
        rest = rest[cut..].trim_start_matches('\n');
    }
    if !rest.is_empty() || parts.is_empty() {
        parts.push(rest.to_owned());
    }
    parts
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn markdown_becomes_mrkdwn() {
        assert_eq!(mrkdwn("## Summary"), "*Summary*");
        assert_eq!(mrkdwn("a **bold** move"), "a *bold* move");
        assert_eq!(mrkdwn("~~gone~~"), "~gone~");
        assert_eq!(
            mrkdwn("see [the PR](https://github.com/x/y/pull/1)"),
            "see <https://github.com/x/y/pull/1|the PR>"
        );
        assert_eq!(mrkdwn("- one\n  - two"), "• one\n  • two");
        assert_eq!(mrkdwn("1 < 2 & 3 > 2"), "1 &lt; 2 &amp; 3 &gt; 2");
        assert_eq!(mrkdwn("`a<b`"), "`a&lt;b`");
    }

    #[test]
    fn code_blocks_are_left_alone_apart_from_escaping() {
        assert_eq!(
            mrkdwn("```rust\nlet **x** = a<b;\n```"),
            "```\nlet **x** = a&lt;b;\n```"
        );
    }

    #[test]
    fn a_link_to_anything_but_the_web_stays_text() {
        assert_eq!(mrkdwn("[x](javascript:alert)"), "[x](javascript:alert)");
    }

    #[test]
    fn long_text_splits_on_lines_under_the_limit() {
        let text = "aaaa\nbbbb\ncccc";
        assert_eq!(split(text, 9), ["aaaa\nbbbb", "cccc"]);
        assert_eq!(split("abcdefgh", 3), ["abc", "def", "gh"]);
        assert_eq!(split("", 3), [""]);
    }
}
