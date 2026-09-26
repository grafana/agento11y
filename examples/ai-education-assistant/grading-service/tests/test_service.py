import traffic
from app import main as service
from fastapi.testclient import TestClient

client = TestClient(service.app)


def test_health_and_students_use_synthetic_records() -> None:
    assert client.get("/health").json() == {"status": "ok"}
    students = client.get("/api/students").json()
    assert students
    assert all(student["id"].startswith("demo-student-") for student in students)


def test_fixture_endpoint_is_disabled_by_default(monkeypatch) -> None:
    monkeypatch.setattr(service, "ENABLE_TEST_FIXTURES", False)
    response = client.post(
        "/api/test-fixtures",
        json={
            "student_id": "demo-student-passing",
            "assignments": [{"assignment_id": "quiz-01", "earned_points": 18}],
        },
    )
    assert response.status_code == 404


def test_fixture_endpoint_validates_scores(monkeypatch) -> None:
    monkeypatch.setattr(service, "ENABLE_TEST_FIXTURES", True)
    response = client.post(
        "/api/test-fixtures",
        json={
            "student_id": "demo-student-passing",
            "assignments": [
                {
                    "assignment_id": "quiz-01",
                    "earned_points": 21,
                    "possible_points": 20,
                }
            ],
        },
    )
    assert response.status_code == 422


def test_unknown_student_returns_not_found_before_failure(monkeypatch) -> None:
    monkeypatch.setattr(service, "ASSIGNMENT_STORE_FAILURE", True)
    response = client.get("/api/assignments?student_id=demo-student-unknown")
    assert response.status_code == 404


def test_assignment_ids_are_validated() -> None:
    response = client.get("/api/assignments/not-valid")
    assert response.status_code == 422


def test_traffic_generator_stops_cleanly(monkeypatch) -> None:
    def interrupt() -> None:
        raise KeyboardInterrupt

    monkeypatch.setattr(traffic, "generate_request_batch", interrupt)
    traffic.main()
