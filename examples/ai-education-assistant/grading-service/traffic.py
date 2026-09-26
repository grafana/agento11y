"""Small read-only traffic generator for the grading service."""

import os
import random
import time

import httpx

BASE_URL = os.getenv("GRADING_SERVICE_URL", "http://127.0.0.1:18181").rstrip("/")


def generate_request_batch() -> None:
    response = httpx.get(f"{BASE_URL}/api/assignments", timeout=5)
    response.raise_for_status()
    assignments = response.json()
    if assignments:
        assignment_id = random.choice(assignments)["id"]
        httpx.get(f"{BASE_URL}/api/assignments/{assignment_id}", timeout=5).raise_for_status()
    httpx.get(f"{BASE_URL}/api/dashboard-summary", timeout=5).raise_for_status()


def main() -> None:
    try:
        while True:
            try:
                generate_request_batch()
            except httpx.HTTPError as error:
                print(f"traffic request failed: {type(error).__name__}")
            time.sleep(2)
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
