"""Render simple dashboard specs to PNG files with matplotlib."""

from __future__ import annotations

from pathlib import Path
from typing import Any

import matplotlib

matplotlib.use("Agg")
from matplotlib import pyplot as plt  # noqa: E402


def render_dashboard(spec: dict[str, Any], path: str) -> None:
    """Renders a line or bar dashboard image to ``path``."""

    series = spec.get("series") if isinstance(spec.get("series"), list) else []
    x_labels = spec.get("x_labels") if isinstance(spec.get("x_labels"), list) else []
    title = str(spec.get("title", "Generated dashboard"))
    subtitle = str(spec.get("subtitle", ""))
    chart_type = str(spec.get("chart_type", "line")).lower()

    fig, ax = plt.subplots(figsize=(10, 5.6), dpi=160)
    fig.patch.set_facecolor("#f7f8fb")
    ax.set_facecolor("#ffffff")
    ax.grid(True, axis="y", color="#e5e7eb", linewidth=0.8)
    ax.spines["top"].set_visible(False)
    ax.spines["right"].set_visible(False)

    plotted = False
    for index, item in enumerate(series):
        if not isinstance(item, dict):
            continue
        values = item.get("values") if isinstance(item.get("values"), list) else []
        y_values = [float(value) for value in values if isinstance(value, (int, float))]
        if not y_values:
            continue
        x_values = list(range(len(y_values)))
        label = str(item.get("name", f"series {index + 1}"))
        if chart_type == "bar":
            width = 0.32
            offset = (index - (len(series) - 1) / 2) * width
            ax.bar([x + offset for x in x_values], y_values, width=width, label=label)
        else:
            ax.plot(x_values, y_values, marker="o", linewidth=2.2, label=label)
        plotted = True

    threshold = spec.get("threshold")
    if isinstance(threshold, (int, float)):
        ax.axhline(float(threshold), color="#dc2626", linestyle="--", linewidth=1.4, label=f"threshold {threshold:g}")

    if x_labels:
        ax.set_xticks(range(len(x_labels)))
        ax.set_xticklabels([str(label) for label in x_labels], rotation=0)
    ax.set_xlabel(str(spec.get("x_label", "")))
    ax.set_ylabel(str(spec.get("y_label", "")))
    ax.set_title(title, loc="left", fontsize=15, fontweight="bold", pad=18)
    if subtitle:
        ax.text(0, 1.02, subtitle, transform=ax.transAxes, fontsize=10, color="#4b5563")
    if plotted:
        ax.legend(loc="best", frameon=False)

    fig.tight_layout()
    Path(path).parent.mkdir(parents=True, exist_ok=True)
    fig.savefig(path, bbox_inches="tight")
    plt.close(fig)
