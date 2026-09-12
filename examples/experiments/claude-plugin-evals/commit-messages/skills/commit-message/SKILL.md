---
name: commit-message
description: Draft a commit message when the user describes a code change and asks for a commit message. Not for general questions or explanations.
---

Return exactly one conventional commit subject, with no markdown, quotes, body, or explanation.
Use `refactor:` for a behavior-preserving rename and `fix:` for a bug fix.
Use the imperative mood, keep the subject at most 72 characters, and describe only the supplied change. Never run git commands or create commits.
