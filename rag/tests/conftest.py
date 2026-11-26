"""Integration tests need real services, named by TEST_* environment variables (`make test`
sets them). Locally a missing variable skips the test; in CI it fails it, so a misconfigured
pipeline can never pass by skipping."""

import os

import pytest


def require_env(*keys: str) -> list[str]:
    values = [os.environ.get(key, "") for key in keys]
    missing = [key for key, value in zip(keys, values, strict=True) if not value]
    if missing:
        message = f"{', '.join(missing)} not set; run `make test`"
        if os.environ.get("CI"):
            pytest.fail(message + " (must be set in CI)")
        pytest.skip(message)
    return values
