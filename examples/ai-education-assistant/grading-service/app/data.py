"""Synthetic records used by the grading service example."""

from copy import deepcopy
from dataclasses import dataclass


@dataclass
class Assignment:
    id: str
    title: str
    earned_points: int
    possible_points: int
    feedback: str
    missed_areas: list[str]
    questions: list[dict[str, object]]


ASSIGNMENTS: dict[str, Assignment] = {
    "quiz-01": Assignment(
        "quiz-01",
        "First Quiz: Ancient Civilizations",
        8,
        20,
        "Review primary sources and compare how geography shaped early societies.",
        [
            "Mesopotamia and river valleys",
            "Comparing primary sources",
            "Early legal systems",
        ],
        [
            {
                "number": 1,
                "prompt": "Which rivers supported Mesopotamian civilization?",
                "earned_points": 2,
                "possible_points": 2,
            },
            {
                "number": 2,
                "prompt": "What was the purpose of Hammurabi's Code?",
                "earned_points": 0,
                "possible_points": 2,
            },
            {
                "number": 3,
                "prompt": "Identify one feature of a primary source.",
                "earned_points": 2,
                "possible_points": 2,
            },
            {
                "number": 4,
                "prompt": "Compare Egyptian and Mesopotamian geography.",
                "earned_points": 0,
                "possible_points": 2,
            },
            {
                "number": 5,
                "prompt": "What made the Nile predictable for farming?",
                "earned_points": 2,
                "possible_points": 2,
            },
            {
                "number": 6,
                "prompt": "Define city-state.",
                "earned_points": 1,
                "possible_points": 2,
            },
            {
                "number": 7,
                "prompt": "How did writing support early governments?",
                "earned_points": 1,
                "possible_points": 2,
            },
            {
                "number": 8,
                "prompt": "Explain one early legal principle.",
                "earned_points": 0,
                "possible_points": 2,
            },
            {
                "number": 9,
                "prompt": "Name one reason civilizations formed near rivers.",
                "earned_points": 0,
                "possible_points": 2,
            },
            {
                "number": 10,
                "prompt": "Connect geography to one social development.",
                "earned_points": 0,
                "possible_points": 2,
            },
        ],
    ),
    "essay-01": Assignment(
        "essay-01",
        "Short Essay: The Roman Republic",
        6,
        10,
        "The thesis needs more support from the assigned readings.",
        ["Citing evidence", "Historical argument"],
        [],
    ),
    "quiz-02": Assignment(
        "quiz-02",
        "Second Quiz: Medieval Trade",
        16,
        20,
        "Good understanding of trade routes; revisit cultural exchange examples.",
        ["Silk Road cultural exchange"],
        [],
    ),
    "project-01": Assignment(
        "project-01",
        "Map Project: Historical Trade Routes",
        27,
        30,
        "Excellent visual explanation and clear chronology.",
        ["Chronology"],
        [],
    ),
}

STUDENTS = {
    "demo-student-low-score": "Low Score Student",
    "demo-student-outage": "Outage Student",
    "demo-student-passing": "Passing Student",
    "demo-student-mixed": "Mixed Scores Student",
}

STUDENT_ASSIGNMENTS = {
    student_id: {key: deepcopy(value) for key, value in ASSIGNMENTS.items()} for student_id in STUDENTS
}

for student_id in ("demo-student-outage", "demo-student-passing"):
    for assignment in STUDENT_ASSIGNMENTS[student_id].values():
        assignment.earned_points = assignment.possible_points
        assignment.feedback = "Strong work. Keep building on this understanding."
        assignment.missed_areas = []
        assignment.questions = []

MIXED_FEEDBACK = {
    "quiz-01": (
        17,
        "Review how geography influenced political organization.",
        ["Geography and political organization"],
    ),
    "essay-01": (
        8,
        "Add one more example from the readings to strengthen the evidence.",
        ["Supporting evidence"],
    ),
    "quiz-02": (
        17,
        "Revisit how trade contributed to cultural exchange.",
        ["Silk Road cultural exchange"],
    ),
    "project-01": (
        27,
        "Add a note explaining why each route became historically important.",
        ["Historical significance"],
    ),
}

for assignment_id, (earned_points, feedback, missed_areas) in MIXED_FEEDBACK.items():
    assignment = STUDENT_ASSIGNMENTS["demo-student-mixed"][assignment_id]
    assignment.earned_points = earned_points
    assignment.feedback = feedback
    assignment.missed_areas = missed_areas
    assignment.questions = []

outage_quiz = STUDENT_ASSIGNMENTS["demo-student-outage"]["quiz-02"]
outage_quiz.earned_points = 18
outage_quiz.feedback = "Strong work overall. Review cultural exchange."
outage_quiz.missed_areas = ["Silk Road cultural exchange"]
outage_quiz.questions = [
    {
        "number": number,
        "prompt": prompt,
        "earned_points": 0 if number == 7 else 2,
        "possible_points": 2,
    }
    for number, prompt in enumerate(
        [
            "Which goods traveled along the Silk Road?",
            "What was the main purpose of caravanserai?",
            "Name one city along a major trade route.",
            "How did merchants travel across deserts?",
            "What is cultural diffusion?",
            "Identify one traded technology.",
            "How did trade spread religious ideas?",
            "Why were oasis towns important?",
            "What role did geography play in trade?",
            "Name one long-term effect of trade.",
        ],
        start=1,
    )
]
