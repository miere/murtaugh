# Murtaugh Roadmap

## 1. Thread status indicators

Send `assistant.threads.setStatus` messages to the thread so users can see the assistant is working on their request.

## 2. Assistant blocks for task execution

Use Slack assistant blocks (e.g., `assistant.threads.setTitle`, `assistant.threads.setSuggestedPrompts`) to provide richer UI when the agent is executing tasks.

## 3. Multiple agent profiles

Allow configuration of multiple ACP agent profiles with different commands, working directories, and personality prompts. Route different Slack channels or users to different profiles.

## 4. Custom link unfurling

Design and implement custom Slack link unfurling handlers so Murtaugh can provide rich previews for specific URL patterns.

## 5. Custom slash commands

Extend slash command handling beyond the built-in `/murtaugh chat` to allow user-defined slash commands with custom workflows.
