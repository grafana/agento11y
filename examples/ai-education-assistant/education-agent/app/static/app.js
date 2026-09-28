const messages = document.querySelector("#messages");
const input = document.querySelector("#input");
const student = document.querySelector("#student");
const form = document.querySelector("#form");
const assignments = document.querySelector("#assignments");
let selected = "demo-student-low-score";
let session = createSession();

function createSession() {
  const suffix = crypto.randomUUID ? crypto.randomUUID() : Date.now();
  return `education-${selected}-${suffix}`;
}

function createElement(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

const SVG_NS = "http://www.w3.org/2000/svg";
const THUMB_UP_PATH =
  "M1 21h4V9H1v12zm22-11c0-1.1-.9-2-2-2h-6.31l.95-4.57.03-.32c0-.41-.17-.79-.44-1.06L14.17 1 7.59 7.59C7.22 7.95 7 8.45 7 9v10c0 1.1.9 2 2 2h9c.83 0 1.54-.5 1.84-1.22l3.02-7.05c.09-.23.14-.47.14-.73v-1.91l-.01-.01L23 10z";
const THUMB_DOWN_PATH =
  "M15 3H6c-.83 0-1.54.5-1.84 1.22l-3.02 7.05c-.09.23-.14.47-.14.73v1.91l.01.01L1 14c0 1.1.89 2 2 2h6.31l-.95 4.57-.03.32c0 .41.17.79.44 1.06L9.83 23l6.59-6.59c.36-.36.58-.86.58-1.41V5c0-1.1-.9-2-2-2zm4 0v12h4V3h-4z";

function createIcon(pathData) {
  const svg = document.createElementNS(SVG_NS, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("width", "14");
  svg.setAttribute("height", "14");
  svg.setAttribute("fill", "currentColor");
  svg.setAttribute("aria-hidden", "true");
  const path = document.createElementNS(SVG_NS, "path");
  path.setAttribute("d", pathData);
  svg.appendChild(path);
  return svg;
}

function addMessage(text, kind) {
  const bubble = createElement("div", `message ${kind}`, text);
  messages.appendChild(bubble);
  messages.scrollTop = messages.scrollHeight;
  return bubble;
}

async function requestJson(url, options) {
  const response = await fetch(url, options);
  if (!response.ok) throw new Error(`request failed: ${response.status}`);
  return response.json();
}

function renderQuestion(question) {
  const row = createElement("div", "row");
  row.style.cssText = "padding:5px 0;font-size:11px;color:#a8aaad";
  row.append(
    createElement("span", "", `Q${question.number} - ${question.prompt}`),
    createElement("span", "", `${question.earned_points}/${question.possible_points}`),
  );
  return row;
}

function renderAssignment(item) {
  const article = createElement("article", "assignment");
  const row = createElement("div", "row");
  row.append(
    createElement("div", "title", item.title),
    createElement(
      "div",
      "score",
      `${item.earned_points}/${item.possible_points} - ${item.percentage}%`,
    ),
  );
  const bar = createElement("div", "bar");
  const fill = createElement("div", "fill");
  fill.style.width = `${Math.max(0, Math.min(100, item.percentage))}%`;
  bar.appendChild(fill);
  article.append(row, bar, createElement("div", "feedback", item.feedback));
  if (item.questions?.length) {
    const questions = createElement("div");
    questions.style.marginTop = "12px";
    item.questions.forEach((question) => questions.appendChild(renderQuestion(question)));
    article.appendChild(questions);
  }
  return article;
}

async function loadAssignments() {
  assignments.replaceChildren(createElement("div", "subtle", "Loading assignments..."));
  try {
    const items = await requestJson(`/api/assignments?student_id=${encodeURIComponent(selected)}`);
    assignments.replaceChildren(...items.map(renderAssignment));
  } catch (_error) {
    assignments.replaceChildren(createElement("div", "fb-error", "Could not load assignments."));
  }
}

function showFeedbackResult(bubble, text, className = "fb-sent") {
  bubble.appendChild(createElement("div", className, text));
}

async function sendFeedback(bubble, row, rating, comment) {
  row.querySelectorAll("button").forEach((button) => { button.disabled = true; });
  try {
    await requestJson("/api/feedback", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ session_id: session, student_id: selected, rating, comment }),
    });
    row.remove();
    showFeedbackResult(bubble, "Thanks for the feedback");
  } catch (_error) {
    row.querySelectorAll("button").forEach((button) => { button.disabled = false; });
    showFeedbackResult(bubble, "Could not record feedback. Try again.", "fb-error");
  }
}

function showCommentBox(bubble, row) {
  const box = createElement("div", "fb-comment");
  const textarea = document.createElement("textarea");
  textarea.placeholder = "What went wrong? (optional)";
  const actions = createElement("div", "fb-comment-actions");
  const skip = createElement("button", "fb-skip", "Skip");
  const send = createElement("button", "fb-send", "Send feedback");
  skip.type = send.type = "button";
  actions.append(skip, send);
  box.append(textarea, actions);
  bubble.appendChild(box);
  skip.onclick = () => { box.remove(); sendFeedback(bubble, row, "bad", ""); };
  send.onclick = () => { box.remove(); sendFeedback(bubble, row, "bad", textarea.value.trim()); };
}

function attachFeedback(bubble) {
  const row = createElement("div", "fb-row");
  const good = createElement("button", "fb-btn fb-up");
  const bad = createElement("button", "fb-btn fb-down");
  good.appendChild(createIcon(THUMB_UP_PATH));
  bad.appendChild(createIcon(THUMB_DOWN_PATH));
  good.type = bad.type = "button";
  good.title = "Good response";
  bad.title = "Bad response";
  row.append(good, bad);
  bubble.appendChild(row);
  good.onclick = () => sendFeedback(bubble, row, "good", "");
  bad.onclick = () => {
    row.querySelectorAll("button").forEach((button) => { button.disabled = true; });
    showCommentBox(bubble, row);
  };
}

async function loadStudents() {
  try {
    const items = await requestJson("/api/students");
    const options = items.map((item) => {
      const option = createElement("option", "", item.name);
      option.value = item.id;
      return option;
    });
    student.replaceChildren(...options);
    student.value = selected;
    await loadAssignments();
  } catch (_error) {
    student.replaceChildren(createElement("option", "", "Students unavailable"));
  }
}

student.onchange = () => {
  selected = student.value;
  session = createSession();
  messages.replaceChildren();
  loadAssignments();
};

form.onsubmit = async (event) => {
  event.preventDefault();
  const text = input.value.trim();
  if (!text) return;
  input.value = "";
  const button = form.querySelector("button");
  button.disabled = true;
  addMessage(text, "user");
  const pending = addMessage("Processing", "assistant processing");
  try {
    const data = await requestJson("/api/chat", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ message: text, session_id: session, student_id: selected }),
    });
    pending.remove();
    const reply = addMessage(data.message, "assistant");
    attachFeedback(reply);
  } catch (_error) {
    pending.remove();
    addMessage("The assistant is temporarily unavailable.", "assistant");
  } finally {
    button.disabled = false;
  }
};

loadStudents();
