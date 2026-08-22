---
name: codereview
description: Reviews code without modifying files. Use when the user explicitly invokes /codereview.
disable-model-invocation: true
---

# Code Review

- Do not modify files.
- Report only actionable correctness, security, concurrency, performance, or test findings.
- Order findings by Critical, High, Medium, then Low severity.
- For each finding, cite the file and line, explain the failure scenario, and suggest the smallest correction.
- If there are no findings, state that clearly and mention any testing gaps.
