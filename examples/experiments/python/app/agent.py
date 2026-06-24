"""A deliberately tiny, framework-free agent: it answers a question.

When ``OPENAI_API_KEY`` is set, the agent calls a real OpenAI chat model.
Otherwise it falls back to a deterministic offline "model" (a dict of canned
answers).

The point of the example is the *experiment plumbing*, not the agent — swap this
out for your real agent and record its call via ``run.start_generation(...)``.
"""

from __future__ import annotations

import os


def answer_question(question: str, *, canned: dict[str, str]) -> str:
    """Returns an answer for ``question``.

    Uses OpenAI when ``OPENAI_API_KEY`` is set; otherwise returns the canned
    answer for the question (falling back to an empty string).
    """

    if os.environ.get("OPENAI_API_KEY"):
        from openai import OpenAI

        client = OpenAI()
        response = client.chat.completions.create(
            model="gpt-4o-mini",
            temperature=0,
            messages=[{"role": "user", "content": question}],
        )
        return (response.choices[0].message.content or "").strip()

    return canned.get(question, "")
