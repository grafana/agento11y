"""Real Anthropic target + stored correctness judge + local rubric + deterministic check.

Makes paid model calls. Configure ingest, control-plane and OTLP credentials.
Install agento11y, agento11y-anthropic and anthropic from compatible artifacts.
"""

import os

from agento11y.experiments import (
    Check,
    Client,
    EvaluationPlan,
    LLMJudge,
    RegexJudge,
    StoredEvaluator,
    TestSuite,
    run_evals,
    setup_evals,
    text_case,
)
from agento11y_anthropic import messages
from anthropic import Anthropic


def main():
    telemetry = setup_evals()
    sdk = Client(
        os.environ["AGENTO11Y_ENDPOINT"],
        ingest_token=os.environ["AGENTO11Y_AUTH_TOKEN"],
        tenant_id=os.getenv("AGENTO11Y_AUTH_TENANT_ID", ""),
    )
    api = Anthropic()
    model = os.getenv("AGENT_MODEL", "claude-haiku-4-5-20251001")
    judge_model = os.getenv("JUDGE_MODEL", "claude-haiku-4-5-20251001")
    suite = TestSuite(
        "reference-and-rubric",
        version="1",
        test_cases=[
            text_case(
                "capital", "What is the capital of France?", expected="Paris", rubric="Answer in one short sentence."
            ),
            text_case("arithmetic", "What is two plus two?", expected="4", rubric="Answer in one short sentence."),
        ],
    )

    def target(prompt):
        request = dict(model=model, max_tokens=128, messages=[{"role": "user", "content": prompt}])
        # Conversation context comes from run_evals; the wrapper exports real usage and live OTel spans.
        response = messages.create(sdk.core, request, lambda req: api.messages.create(**req))
        return "".join(part.text for part in response.content if part.type == "text")

    def local_judge(prompt):
        # LLMJudge records this response and its usage as separate grader evidence.
        return api.messages.create(model=judge_model, max_tokens=256, messages=[{"role": "user", "content": prompt}])

    plan = EvaluationPlan(
        [
            Check(
                "correctness", StoredEvaluator.llm_judge("demo.correctness", provider="anthropic", model=judge_model)
            ),
            Check(
                "rubric",
                LLMJudge.for_case(
                    "demo.rubric", local_judge, model_provider="anthropic", model_name=judge_model, mode="rubric"
                ),
            ),
            Check("nonempty", RegexJudge("demo.nonempty", r"\S")),
        ]
    )
    try:
        run = run_evals(
            suite,
            target,
            client=sdk,
            plan=plan,
            name="Reference + rubric + deterministic checks",
            candidate={"agent_name": "qa", "model_provider": "anthropic", "model_name": model},
        )
        print(run.url)
    finally:
        api.close()
        sdk.shutdown()
        telemetry.shutdown()


if __name__ == "__main__":
    main()
