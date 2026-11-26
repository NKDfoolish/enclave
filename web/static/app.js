// Enclave web UI: login, workspaces, members, invitations, documents and questions.
// Plain DOM APIs only. User data is always inserted with textContent, never innerHTML,
// so names and emails cannot inject markup (XSS).

const views = ["auth-view", "accept-view", "workspaces-view", "workspace-view"];
const $ = (id) => document.getElementById(id);

let currentUser = null;
let currentWorkspace = null;
let currentConversation = null;
let pendingInviteToken = readInviteToken();

class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

function send(method, path, body) {
  const options = { method, headers: {} };
  if (body !== undefined) {
    options.headers["Content-Type"] = "application/json";
    options.body = JSON.stringify(body);
  }
  return fetch(path, options);
}

// Access tokens live 15 minutes. When one expires the API answers 401; we then ask for new
// tokens once and retry. Concurrent 401s share a single refresh request.
let refreshInFlight = null;

function refreshTokens() {
  refreshInFlight ??= fetch("/v1/auth/refresh", { method: "POST" })
    // 409: another tab refreshed a moment ago, so the shared cookies are already fresh.
    .then((response) => response.ok || response.status === 409)
    .finally(() => {
      refreshInFlight = null;
    });
  return refreshInFlight;
}

async function api(method, path, body) {
  let response = await send(method, path, body);
  if (response.status === 401 && !path.startsWith("/v1/auth/") && (await refreshTokens())) {
    response = await send(method, path, body);
  }
  if (response.status === 204) {
    return null;
  }
  const data = await response.json().catch(() => null);
  if (!response.ok) {
    throw new ApiError(response.status, data?.error?.message ?? response.statusText);
  }
  return data;
}

// The invite token travels in the URL fragment (#accept=...), which browsers never send to
// the server. Remove it from the address bar once read.
function readInviteToken() {
  const match = location.hash.match(/^#accept=(.+)$/);
  if (!match) {
    return null;
  }
  history.replaceState(null, "", location.pathname);
  return match[1];
}

function showView(id) {
  for (const view of views) {
    $(view).hidden = view !== id;
  }
}

function notify(message, isError = false) {
  const notice = $("notice");
  notice.textContent = message;
  notice.classList.toggle("error", isError);
  notice.hidden = !message;
}

function element(tag, text = "", className = "") {
  const node = document.createElement(tag);
  node.textContent = text;
  if (className) {
    node.className = className;
  }
  return node;
}

function button(label, onClick, className = "") {
  const node = element("button", label, className);
  node.type = "button";
  node.addEventListener("click", onClick);
  return node;
}

// Runs an action and reports its error, so every click handler fails visibly.
async function attempt(action) {
  try {
    notify("");
    await action();
  } catch (error) {
    if (error instanceof ApiError && error.status === 401) {
      await showLoggedOut();
    }
    notify(error.message, true);
  }
}

// --- Session ---------------------------------------------------------------------------

async function start() {
  try {
    currentUser = await api("GET", "/v1/me");
  } catch (error) {
    await showLoggedOut();
    if (pendingInviteToken) {
      notify("Log in or create an account to accept your invitation.");
    }
    return;
  }
  await showLoggedIn();
}

async function showLoggedOut() {
  currentUser = null;
  currentWorkspace = null;
  $("session-bar").hidden = true;
  showView("auth-view");
}

async function showLoggedIn() {
  $("session-email").textContent = currentUser.email;
  $("session-bar").hidden = false;
  if (pendingInviteToken) {
    showView("accept-view");
    return;
  }
  await showWorkspaces();
}

$("login-form").addEventListener("submit", (event) => {
  event.preventDefault();
  const form = new FormData(event.target);
  attempt(async () => {
    currentUser = await api("POST", "/v1/auth/login", {
      email: form.get("email"),
      password: form.get("password"),
    });
    event.target.reset();
    await showLoggedIn();
  });
});

$("register-form").addEventListener("submit", (event) => {
  event.preventDefault();
  const form = new FormData(event.target);
  attempt(async () => {
    await api("POST", "/v1/auth/register", {
      display_name: form.get("display_name"),
      email: form.get("email"),
      password: form.get("password"),
    });
    currentUser = await api("POST", "/v1/auth/login", {
      email: form.get("email"),
      password: form.get("password"),
    });
    event.target.reset();
    await showLoggedIn();
  });
});

$("logout-button").addEventListener("click", () =>
  attempt(async () => {
    await api("POST", "/v1/auth/logout");
    await showLoggedOut();
  }),
);

// --- Invitations -----------------------------------------------------------------------

$("accept-button").addEventListener("click", () =>
  attempt(async () => {
    const token = pendingInviteToken;
    pendingInviteToken = null;
    const workspace = await api("POST", "/v1/invitations/accept", { token });
    notify(`You joined ${workspace.name} as ${workspace.role}.`);
    await openWorkspace(workspace.id);
  }),
);

$("decline-button").addEventListener("click", () =>
  attempt(async () => {
    pendingInviteToken = null;
    await showWorkspaces();
  }),
);

// --- Workspaces ------------------------------------------------------------------------

async function showWorkspaces() {
  currentWorkspace = null;
  const { workspaces } = await api("GET", "/v1/workspaces");
  const list = $("workspace-list");
  list.replaceChildren();
  if (workspaces.length === 0) {
    list.append(element("li", "You are not in any workspace yet.", "muted"));
  }
  for (const workspace of workspaces) {
    const item = element("li");
    item.append(
      button(workspace.name, () => attempt(() => openWorkspace(workspace.id)), "link"),
      element("span", workspace.role, "badge"),
    );
    list.append(item);
  }
  showView("workspaces-view");
}

$("create-workspace-form").addEventListener("submit", (event) => {
  event.preventDefault();
  const form = new FormData(event.target);
  attempt(async () => {
    const workspace = await api("POST", "/v1/workspaces", { name: form.get("name") });
    event.target.reset();
    await openWorkspace(workspace.id);
  });
});

$("back-button").addEventListener("click", () => attempt(showWorkspaces));

async function openWorkspace(workspaceID) {
  currentWorkspace = await api("GET", `/v1/workspaces/${workspaceID}`);
  $("workspace-title").textContent = currentWorkspace.name;
  $("workspace-role").textContent = currentWorkspace.role;
  $("invite-link").hidden = true;

  const isAdmin = currentWorkspace.role === "admin";
  const canViewMembers = isAdmin || currentWorkspace.role === "staff";
  $("members-panel").hidden = !canViewMembers;
  $("ai-panel").hidden = !canViewMembers; // staff see the AI policy, only admins change it
  $("admin-panel").hidden = !isAdmin;

  $("upload-form").hidden = !canViewMembers; // admins and staff manage documents
  await loadDocuments();
  if (canViewMembers) {
    await loadMembers(isAdmin);
    await loadAISettings(isAdmin);
  }
  if (isAdmin) {
    await loadInvitations();
  }
  currentConversation = null;
  await loadConversations();
  showView("workspace-view");
}

// --- Documents ----------------------------------------------------------------------------

let documentsCursor = "";
let documentsPoll = null;

function canManageDocuments() {
  return currentWorkspace.role === "admin" || currentWorkspace.role === "staff";
}

function documentsPath(suffix = "") {
  return `/v1/workspaces/${currentWorkspace.id}/documents${suffix}`;
}

async function loadDocuments(append = false) {
  const cursor = append ? `?cursor=${encodeURIComponent(documentsCursor)}` : "";
  const { documents, next_cursor } = await api("GET", documentsPath(cursor));
  const rows = $("document-rows");
  if (!append) {
    rows.replaceChildren();
  }
  if (!append && documents.length === 0) {
    const empty = element("tr");
    const cell = element("td", "No documents yet.", "muted");
    cell.colSpan = 5;
    empty.append(cell);
    rows.append(empty);
  }
  for (const doc of documents) {
    rows.append(documentRow(doc));
  }
  documentsCursor = next_cursor;
  $("more-documents").hidden = !next_cursor;
  scheduleDocumentsPoll(documents);
}

$("more-documents").addEventListener("click", () => attempt(() => loadDocuments(true)));

function documentRow(doc) {
  const row = element("tr");
  const title = element("td", doc.title);
  title.title = doc.filename;
  const status = element("td");
  status.append(element("span", doc.status, "badge"));
  if (doc.failure_reason) {
    status.append(element("div", doc.failure_reason, "muted"));
  }
  // Whether questions can find the document yet (Part 4).
  if (doc.index_status) {
    const label = { pending: "indexing…", indexed: "searchable", failed: "not searchable" }[doc.index_status];
    status.append(element("div", label, "muted"));
    if (doc.index_error) {
      status.append(element("div", doc.index_error, "muted"));
    }
  }

  const actions = element("td");
  if (doc.status === "ready") {
    actions.append(button("Download", () => attempt(() => download(doc))));
  }
  if (canManageDocuments()) {
    actions.append(button("Delete", () => deleteDocument(doc), "danger"));
  }

  row.append(title, element("td", formatSize(doc.size_bytes)), status, visibilityCell(doc), actions);
  return row;
}

function visibilityCell(doc) {
  const cell = element("td");
  const label = { internal: "Staff only", client_common: "Staff and clients" };
  if (!canManageDocuments()) {
    cell.textContent = label[doc.visibility];
    return cell;
  }
  const select = element("select");
  for (const value of ["internal", "client_common"]) {
    const option = element("option", label[value]);
    option.value = value;
    option.selected = value === doc.visibility;
    select.append(option);
  }
  select.addEventListener("change", () =>
    attempt(async () => {
      await api("PATCH", documentsPath(`/${doc.id}`), { visibility: select.value });
      notify(`"${doc.title}" is now visible to ${label[select.value].toLowerCase()}.`);
    }),
  );
  cell.append(select);
  return cell;
}

// The API answers /download with a redirect to a 60-second presigned URL on the storage
// server. Loading the document first refreshes an expired access token, then the browser
// navigates and saves the file.
async function download(doc) {
  await api("GET", documentsPath(`/${doc.id}`));
  location.href = documentsPath(`/${doc.id}/download`);
}

async function deleteDocument(doc) {
  if (!confirm(`Delete "${doc.title}"? This cannot be undone.`)) {
    return;
  }
  await attempt(async () => {
    await api("DELETE", documentsPath(`/${doc.id}`));
    notify(`"${doc.title}" was deleted.`);
    await loadDocuments();
  });
}

// Upload: 1) register the document and get a presigned URL, 2) PUT the file straight to
// object storage, 3) tell the API it is done. The server then checks the content.
$("upload-form").addEventListener("submit", (event) => {
  event.preventDefault();
  const form = event.target;
  const file = form.file.files[0];
  attempt(async () => {
    const { document: doc, upload } = await api("POST", documentsPath(), {
      filename: file.name,
      title: form.title.value,
      size_bytes: file.size,
    });
    const stored = await fetch(upload.url, { method: upload.method, headers: upload.headers, body: file });
    if (!stored.ok) {
      throw new Error(`Upload to storage failed (${stored.status}).`);
    }
    await api("POST", documentsPath(`/${doc.id}/complete`));
    form.reset();
    notify(`"${doc.title}" uploaded; checking the file…`);
    await loadDocuments();
  });
});

// While any listed document is still uploading or processing, refresh the list shortly.
function scheduleDocumentsPoll(documents) {
  clearTimeout(documentsPoll);
  const pending = documents.some(
    (d) => d.status === "uploading" || d.status === "processing" || d.index_status === "pending",
  );
  if (pending && currentWorkspace) {
    const workspaceID = currentWorkspace.id;
    documentsPoll = setTimeout(() => {
      if (currentWorkspace?.id === workspaceID) {
        attempt(() => loadDocuments());
      }
    }, 1500);
  }
}

function formatSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
}

async function loadMembers(isAdmin) {
  const { members } = await api("GET", `/v1/workspaces/${currentWorkspace.id}/members`);
  const rows = $("member-rows");
  rows.replaceChildren();
  for (const member of members) {
    const row = element("tr");
    const roleCell = element("td");
    const actionCell = element("td");

    if (isAdmin) {
      roleCell.append(roleSelect(member));
      actionCell.append(button("Remove", () => removeMember(member), "danger"));
    } else {
      roleCell.textContent = member.role;
    }
    row.append(element("td", member.display_name), element("td", member.email), roleCell, actionCell);
    rows.append(row);
  }
}

function roleSelect(member) {
  const select = element("select");
  for (const role of ["admin", "staff", "client"]) {
    const option = element("option", role);
    option.value = role;
    option.selected = role === member.role;
    select.append(option);
  }
  select.addEventListener("change", () =>
    attempt(async () => {
      try {
        await api("PATCH", `/v1/workspaces/${currentWorkspace.id}/members/${member.user_id}`, {
          role: select.value,
        });
        notify(`${member.display_name} is now ${select.value}.`);
      } finally {
        // Reload so the page reflects what the server accepted, e.g. a refused last-admin change.
        await openWorkspace(currentWorkspace.id);
      }
    }),
  );
  return select;
}

async function removeMember(member) {
  if (!confirm(`Remove ${member.display_name} from ${currentWorkspace.name}?`)) {
    return;
  }
  await attempt(async () => {
    await api("DELETE", `/v1/workspaces/${currentWorkspace.id}/members/${member.user_id}`);
    notify(`${member.display_name} was removed.`);
    await openWorkspace(currentWorkspace.id);
  });
}

// --- AI settings --------------------------------------------------------------------------

async function loadAISettings(isAdmin) {
  const settings = await api("GET", `/v1/workspaces/${currentWorkspace.id}/ai-settings`);
  const options = $("ai-provider-options");
  options.replaceChildren();
  for (const provider of settings.available_external_providers) {
    const checkbox = element("input");
    checkbox.type = "checkbox";
    checkbox.name = "provider";
    checkbox.value = provider;
    checkbox.checked = settings.external_providers.includes(provider);
    checkbox.disabled = !isAdmin;
    const label = element("label");
    label.append(checkbox, ` ${provider}`);
    options.append(label);
  }
  $("ai-settings-save").hidden = !isAdmin;
}

$("ai-settings-form").addEventListener("submit", (event) => {
  event.preventDefault();
  const checked = event.target.querySelectorAll("input[name=provider]:checked");
  const providers = Array.from(checked, (checkbox) => checkbox.value);
  attempt(async () => {
    await api("PUT", `/v1/workspaces/${currentWorkspace.id}/ai-settings`, { external_providers: providers });
    notify(providers.length ? `Allowed: ${providers.join(", ")}.` : "Only local models are allowed now.");
    await loadAISettings(true);
  });
});

async function loadInvitations() {
  const { invitations } = await api("GET", `/v1/workspaces/${currentWorkspace.id}/invitations`);
  const list = $("invitation-list");
  list.replaceChildren();
  if (invitations.length === 0) {
    list.append(element("li", "No pending invitations.", "muted"));
  }
  for (const invitation of invitations) {
    const expires = new Date(invitation.expires_at).toLocaleDateString();
    const item = element("li");
    item.append(
      element("span", `${invitation.email} · ${invitation.role} · expires ${expires}`),
      button("Revoke", () => revokeInvitation(invitation), "danger"),
    );
    list.append(item);
  }
}

$("invite-form").addEventListener("submit", (event) => {
  event.preventDefault();
  const form = new FormData(event.target);
  attempt(async () => {
    const result = await api("POST", `/v1/workspaces/${currentWorkspace.id}/invitations`, {
      email: form.get("email"),
      role: form.get("role"),
    });
    event.target.reset();
    await loadInvitations();
    $("invite-link-url").textContent = location.origin + result.accept_url;
    $("invite-link").hidden = false;
  });
});

async function revokeInvitation(invitation) {
  await attempt(async () => {
    await api("DELETE", `/v1/workspaces/${currentWorkspace.id}/invitations/${invitation.id}`);
    notify(`Invitation for ${invitation.email} was revoked.`);
    await loadInvitations();
  });
}

// Pasting an invite link into an open tab only changes the fragment; there is no page load.
window.addEventListener("hashchange", () => {
  const token = readInviteToken();
  if (token) {
    pendingInviteToken = token;
    attempt(start);
  }
});

start();

// --- Questions and answers --------------------------------------------------------------
// Answers stream as Server-Sent Events from POST .../messages: "sources", many "delta",
// then "done" with the stored answer (or "error"). EventSource only does GET, so the stream
// is read from fetch().

function conversationsPath(suffix = "") {
  return `/v1/workspaces/${currentWorkspace.id}/conversations${suffix}`;
}

async function loadConversations() {
  const { conversations } = await api("GET", conversationsPath());
  const list = $("conversation-list");
  list.replaceChildren();
  for (const conversation of conversations) {
    const item = element("li");
    const open = button(conversation.title || "New conversation", () => attempt(() => openConversation(conversation.id)), "link");
    if (conversation.id === currentConversation) {
      open.classList.add("active");
    }
    item.append(open);
    list.append(item);
  }
  if (!currentConversation) {
    $("chat-messages").replaceChildren(element("p", "Ask a question about this workspace's documents.", "muted"));
  }
}

async function openConversation(conversationID) {
  const { messages } = await api("GET", conversationsPath(`/${conversationID}`));
  currentConversation = conversationID;
  $("chat-messages").replaceChildren(...messages.map(messageNode));
  await loadConversations();
}

function messageNode(message) {
  const node = element("div", "", `message ${message.role}`);
  const text = element("p", message.content, message.hidden ? "muted" : "");
  node.append(text);
  node.append(citationList(message.citations ?? []));
  return node;
}

function citationList(citations) {
  const list = element("ol", "", "citations");
  for (const citation of citations) {
    const item = element("li");
    item.value = citation.number;
    const link = element("a", citation.title);
    // A plain GET: the API checks access again and redirects to a short-lived download URL.
    link.href = documentsPath(`/${citation.document_id}/download`);
    const where = [citation.page ? `page ${citation.page}` : "", citation.section ?? ""].filter(Boolean).join(", ");
    item.append(link);
    if (where) {
      item.append(element("span", ` — ${where}`, "muted"));
    }
    if (citation.snippet) {
      item.title = citation.snippet;
    }
    list.append(item);
  }
  return list;
}

$("new-conversation").addEventListener("click", () =>
  attempt(async () => {
    currentConversation = null;
    await loadConversations();
    $("ask-form").elements.question.focus();
  }),
);

$("ask-form").addEventListener("submit", (event) => {
  event.preventDefault();
  const form = event.target;
  const question = form.elements.question.value.trim();
  if (!question) {
    return;
  }
  form.elements.question.value = "";
  attempt(() => ask(question));
});

async function ask(question) {
  if (!currentConversation) {
    currentConversation = (await api("POST", conversationsPath())).id;
    $("chat-messages").replaceChildren();
  }
  const messages = $("chat-messages");
  messages.append(messageNode({ role: "user", content: question }));
  const answer = element("div", "", "message assistant");
  const text = element("p", "Searching…", "muted");
  answer.append(text);
  messages.append(answer);

  let started = false;
  await streamAnswer(conversationsPath(`/${currentConversation}/messages`), question, (name, data) => {
    if (name === "delta") {
      if (!started) {
        text.textContent = "";
        text.className = "";
        started = true;
      }
      text.textContent += data.text;
    } else if (name === "done") {
      answer.replaceWith(messageNode(data.message));
    } else if (name === "error") {
      text.textContent = data.error.message;
      text.className = "muted";
    }
  });
  await loadConversations();
}

async function streamAnswer(path, question, onEvent) {
  let response = await send("POST", path, { content: question });
  if (response.status === 401 && (await refreshTokens())) {
    response = await send("POST", path, { content: question });
  }
  if (!response.headers.get("Content-Type")?.startsWith("text/event-stream")) {
    const data = await response.json().catch(() => null);
    throw new ApiError(response.status, data?.error?.message ?? response.statusText);
  }
  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
  let buffer = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) {
      return;
    }
    buffer += value;
    let end;
    while ((end = buffer.indexOf("\n\n")) >= 0) {
      const block = buffer.slice(0, end);
      buffer = buffer.slice(end + 2);
      const name = block.match(/^event: (.*)$/m)?.[1];
      const data = block.match(/^data: (.*)$/m)?.[1];
      if (name && data) {
        onEvent(name, JSON.parse(data));
      }
    }
  }
}
