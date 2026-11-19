"""The example uses LangChain's provider defaults for both supported providers."""

from unittest.mock import patch

import pytest
from app.agent import build_agent, get_weather


@pytest.mark.parametrize("model", ["claude-haiku-4-5", "gpt-4.1-mini"])
def test_build_agent_uses_provider_defaults(model):
    with patch("app.agent.init_chat_model") as init, patch("app.agent.create_agent") as create:
        assert build_agent(model) is create.return_value
        init.assert_called_once_with(model)
        assert create.call_args.kwargs["model"] is init.return_value
        assert create.call_args.kwargs["tools"] == [get_weather]
