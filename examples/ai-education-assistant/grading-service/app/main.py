"""In-memory grading service for the AI education assistant example."""

from __future__ import annotations

import atexit
import logging
import os
from dataclasses import asdict
from typing import Annotated
from urllib.parse import unquote

from app.data import ASSIGNMENTS, STUDENT_ASSIGNMENTS, STUDENTS, Assignment
from dotenv import load_dotenv
from fastapi import FastAPI, HTTPException, Path, Query, Request
from opentelemetry import metrics, trace
from opentelemetry.instrumentation.fastapi import FastAPIInstrumentor
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from pydantic import BaseModel, Field

load_dotenv(override=True)

SERVICE_NAME = os.getenv("OTEL_SERVICE_NAME", "ai-education-grading-service")
ASSIGNMENT_STORE_FAILURE = os.getenv("ASSIGNMENT_STORE_FAILURE", "false").lower() in {
    "1",
    "true",
    "yes",
    "on",
}
ASSIGNMENT_STORE_FAILURE_STUDENT_ID = os.getenv("ASSIGNMENT_STORE_FAILURE_STUDENT_ID", "demo-student-outage")
ENABLE_TEST_FIXTURES = os.getenv("ENABLE_TEST_FIXTURES", "false").lower() in {
    "1",
    "true",
    "yes",
    "on",
}
STUDENT_ID_PATTERN = r"^demo-student-[a-z-]+$"
ASSIGNMENT_ID_PATTERN = r"^[a-z]+-[0-9]{2}$"
DEFAULT_STUDENT_ID = "demo-student-low-score"


def parse_otlp_headers() -> dict[str, str]:
    value = unquote(os.getenv("OTEL_EXPORTER_OTLP_HEADERS", ""))
    raw_headers = value.strip().strip('"').strip("'")
    return {
        key.strip(): header_value.strip()
        for item in raw_headers.split(",")
        if "=" in item
        for key, header_value in [item.split("=", 1)]
    }


def configure_telemetry() -> None:
    endpoint = os.getenv("OTEL_EXPORTER_OTLP_ENDPOINT", "").strip().rstrip("/")
    if not endpoint:
        logging.getLogger(__name__).warning("OTEL disabled: configure OTEL_EXPORTER_OTLP_ENDPOINT")
        return
    from opentelemetry.exporter.otlp.proto.http._log_exporter import OTLPLogExporter
    from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
    from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
    from opentelemetry.sdk._logs import LoggerProvider, LoggingHandler
    from opentelemetry.sdk._logs.export import BatchLogRecordProcessor
    from opentelemetry.sdk.metrics import MeterProvider
    from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader

    resource = Resource.create({"service.name": SERVICE_NAME, "service.version": "0.1.0"})
    headers = parse_otlp_headers()
    provider = TracerProvider(resource=resource)
    provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter(endpoint=f"{endpoint}/v1/traces", headers=headers)))
    trace.set_tracer_provider(provider)
    reader = PeriodicExportingMetricReader(
        OTLPMetricExporter(endpoint=f"{endpoint}/v1/metrics", headers=headers),
        export_interval_millis=int(os.getenv("OTEL_METRIC_EXPORT_INTERVAL_MS", "10000")),
    )
    metrics.set_meter_provider(MeterProvider(resource=resource, metric_readers=[reader]))
    logs = LoggerProvider(resource=resource)
    logs.add_log_record_processor(
        BatchLogRecordProcessor(OTLPLogExporter(endpoint=f"{endpoint}/v1/logs", headers=headers))
    )
    logging.getLogger().addHandler(LoggingHandler(level=logging.INFO, logger_provider=logs))
    logging.getLogger().setLevel(logging.INFO)
    atexit.register(provider.shutdown)
    atexit.register(logs.shutdown)


configure_telemetry()
METER = metrics.get_meter(SERVICE_NAME)
REQUESTS = METER.create_counter("education.grading.requests")
tracer = trace.get_tracer(SERVICE_NAME)
logger = logging.getLogger(__name__)


class AssignmentResponse(BaseModel):
    id: str
    title: str
    earned_points: int
    possible_points: int
    percentage: float
    feedback: str
    missed_areas: list[str]
    questions: list[dict[str, object]]


class SeedAssignment(BaseModel):
    assignment_id: str = Field(pattern=ASSIGNMENT_ID_PATTERN, max_length=64)
    earned_points: int = Field(ge=0, le=10_000)
    possible_points: int | None = Field(default=None, gt=0, le=10_000)
    feedback: str | None = Field(default=None, max_length=1_000)


class SeedScoresRequest(BaseModel):
    student_id: str = Field(pattern=STUDENT_ID_PATTERN, max_length=64)
    assignments: list[SeedAssignment] = Field(min_length=1, max_length=10)


def render(assignment: Assignment) -> AssignmentResponse:
    percentage = round(assignment.earned_points / assignment.possible_points * 100, 1)
    return AssignmentResponse(**asdict(assignment), percentage=percentage)


def trigger_store_failure(student_id: str) -> None:
    with tracer.start_as_current_span("assignment_store") as span:
        with tracer.start_as_current_span("assignment_record_lookup") as lookup_span:
            try:
                list(STUDENT_ASSIGNMENTS[student_id].values())[9]
                raise IndexError("gradebook assignment index out of range")
            except Exception as error:
                lookup_span.record_exception(error)
                lookup_span.set_status(trace.Status(trace.StatusCode.ERROR, type(error).__name__))
                span.record_exception(error)
                span.set_status(trace.Status(trace.StatusCode.ERROR, "assignment store unavailable"))
                logger.error("assignment store lookup failed", exc_info=True)
    raise HTTPException(status_code=503, detail="assignment store unavailable")


app = FastAPI(title="AI Education Grading Service", version="0.1.0")
FastAPIInstrumentor.instrument_app(app)


@app.get("/health")
def health() -> dict[str, str]:
    return {"status": "ok"}


@app.get("/api/students")
def list_students() -> list[dict[str, str]]:
    return [{"id": student_id, "name": name} for student_id, name in STUDENTS.items()]


@app.post("/api/test-fixtures")
def seed_test_fixtures(request: SeedScoresRequest) -> list[AssignmentResponse]:
    """Seed local experiment scores when the fixture API is explicitly enabled."""
    if not ENABLE_TEST_FIXTURES:
        raise HTTPException(status_code=404, detail="not found")
    assignments = STUDENT_ASSIGNMENTS.get(request.student_id)
    if assignments is None:
        raise HTTPException(status_code=404, detail="student not found")
    for seeded in request.assignments:
        assignment = assignments.get(seeded.assignment_id)
        if assignment is None:
            raise HTTPException(status_code=404, detail="assignment not found")
        possible_points = seeded.possible_points or assignment.possible_points
        if seeded.earned_points > possible_points:
            raise HTTPException(status_code=422, detail="earned points exceed possible points")
        assignment.earned_points = seeded.earned_points
        assignment.possible_points = possible_points
        if seeded.feedback is not None:
            assignment.feedback = seeded.feedback
    logger.info("test fixture scores seeded", extra={"assignment_count": len(request.assignments)})
    return [render(item) for item in assignments.values()]


@app.get("/api/assignments", response_model=list[AssignmentResponse])
def list_assignments(
    request: Request,
    student_id: Annotated[str, Query(pattern=STUDENT_ID_PATTERN, max_length=64)] = DEFAULT_STUDENT_ID,
) -> list[AssignmentResponse]:
    REQUESTS.add(1, {"route": "/api/assignments", "method": "GET"})
    assignments = STUDENT_ASSIGNMENTS.get(student_id)
    if assignments is None:
        raise HTTPException(status_code=404, detail="student not found")
    is_agent_request = request.headers.get("x-demo-agent-request") == "true"
    if ASSIGNMENT_STORE_FAILURE or (is_agent_request and student_id == ASSIGNMENT_STORE_FAILURE_STUDENT_ID):
        trigger_store_failure(student_id)
    return [render(item) for item in assignments.values()]


@app.get("/api/assignments/{assignment_id}", response_model=AssignmentResponse)
def get_assignment(
    assignment_id: Annotated[str, Path(pattern=ASSIGNMENT_ID_PATTERN, max_length=64)],
) -> AssignmentResponse:
    REQUESTS.add(1, {"route": "/api/assignments/{assignment_id}", "method": "GET"})
    assignment = ASSIGNMENTS.get(assignment_id)
    if assignment is None:
        raise HTTPException(status_code=404, detail="assignment not found")
    return render(assignment)


@app.get("/api/dashboard-summary")
def dashboard_summary() -> dict[str, float | int]:
    REQUESTS.add(1, {"route": "/api/dashboard-summary", "method": "GET"})
    total_earned = sum(item.earned_points for item in ASSIGNMENTS.values())
    total_possible = sum(item.possible_points for item in ASSIGNMENTS.values())
    below_60 = sum(item.earned_points / item.possible_points < 0.6 for item in ASSIGNMENTS.values())
    return {
        "assignments": len(ASSIGNMENTS),
        "average_percentage": round(total_earned / total_possible * 100, 1),
        "below_60": below_60,
    }
