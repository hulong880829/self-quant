---
name: fixbug
description: Diagnoses and fixes a reported bug with the smallest safe change. Use when the user explicitly invokes /fixbug.
disable-model-invocation: true
---

# Fix Bug

1. Reproduce or confirm the failure and inspect the relevant code path.
2. Identify the root cause using concrete evidence; do not patch symptoms.
3. Implement the smallest root-cause fix without unrelated refactoring or scope expansion.
4. Add or update a focused regression test when practical, and run the minimum validation proportional to the change.
5. Review the final diff for unintended changes.
6. Report the root cause, modified files, validation results, and remaining risks.