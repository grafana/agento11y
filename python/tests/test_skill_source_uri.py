import pytest
from agento11y.client import _sanitize_skill_source_uri


@pytest.mark.parametrize("escape", ["%", "%2", "%zz", "%2g"])
@pytest.mark.parametrize(
    "template",
    [
        "https://user{}:secret@skills.example/review",
        "https://user:secret{}@skills.example/review",
        "https://skills{}.example/review",
        "https://skills.example/review{}",
        "https://skills.example/review?token={}",
        "https://skills.example/review#{}",
        "file:///opt/skills/review{}/SKILL.md",
    ],
)
def test_skill_source_uri_rejects_malformed_percent_escapes(template: str, escape: str) -> None:
    assert _sanitize_skill_source_uri(template.format(escape)) == ""


@pytest.mark.parametrize(
    "char",
    [*(chr(code) for code in range(32)), *(chr(code) for code in range(127, 160)), " ", "\u2003", "\u2028"],
)
def test_skill_source_uri_rejects_whitespace_and_controls(char: str) -> None:
    assert _sanitize_skill_source_uri(f"https://skills.example/re{char}view") == ""


@pytest.mark.parametrize(
    "value",
    [
        "",
        "review/SKILL.md",
        "/opt/skills/review/SKILL.md",
        "file:",
        "file:relative",
        "file://localhost",
        "https:///path",
        "https:/path",
        "https://",
        "https://user:secret@/path",
        "https://[broken/path",
        "https://skills.example:invalid/path",
        "https://skills.example:-1/path",
        "https://skills.example:65536/path",
    ],
)
def test_skill_source_uri_rejects_missing_or_invalid_locations(value: str) -> None:
    assert _sanitize_skill_source_uri(value) == ""


@pytest.mark.parametrize(
    ("value", "expected"),
    [
        ("file:/opt/skills/review/SKILL.md", "file:///opt/skills/review/SKILL.md"),
        ("file:///opt/skills/review/SKILL.md", "file:///opt/skills/review/SKILL.md"),
        ("file:///opt/skills/code%20review/SKILL.md?token=secret#part", "file:///opt/skills/code%20review/SKILL.md"),
        ("file://localhost/opt/skills/review/SKILL.md", "file://localhost/opt/skills/review/SKILL.md"),
        ("https://skills.example", "https://skills.example"),
        (
            "https://user%40name:secret%2F@skills.example:443/code%20review/%25.md?token=secret#part",
            "https://skills.example:443/code%20review/%25.md",
        ),
        ("https://user:secret@[::1]:8443/review?token=secret#part", "https://[::1]:8443/review"),
        ("s3://user:secret@skills/review%2FSKILL.md?version=secret#part", "s3://skills/review%2FSKILL.md"),
        ("gs://skills/review%20notes.md?token=secret#part", "gs://skills/review%20notes.md"),
    ],
)
def test_skill_source_uri_preserves_locations_and_scrubs_private_components(value: str, expected: str) -> None:
    assert _sanitize_skill_source_uri(value) == expected
