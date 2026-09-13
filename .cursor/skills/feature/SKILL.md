---
name: feature
description: Implements a requested feature with minimal scope and appropriate validation. Use when the user explicitly invokes /feature.
disable-model-invocation: true
---

# Feature

1. Understand the request and inspect the related implementation.
2. Clarify only ambiguities that materially affect correctness or design.
3. Implement the smallest complete solution without unrelated refactoring or scope expansion.
4. Run the minimum validation proportional to the change.
5. Review the final diff for unintended changes.
6. Report the behavior, modified files, and validation results.
