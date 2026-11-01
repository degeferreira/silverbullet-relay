---
tags: meta
---

```space-lua
-- priority: 5
hub = {}
hub.tailnet = config.get("relay.legacyServer") or ""

function hub.desktopConnection()
  return clientStore.get("personal-hub.desktop.v1")
end

function hub.remoteCall(path, method, body, token)
  local options = {method = method or "GET", headers = {Authorization = "Bearer " .. token}}
  if body then
    options.body = body
    -- Two proxies: Desktop -> remote SilverBullet -> integration-service.
    options.headers["X-Proxy-Header-Content-Type"] = "application/json"
  end
  local result = net.proxyFetch((hub.connectingServer or (hub.desktopConnection() or {}).server or hub.tailnet) .. "/.proxy/127.0.0.1:8088/v1/" .. path, options)
  if result.status == 200 then
    result.status = tonumber((result.headers or {})["x-proxy-status-code"] or 200)
  end
  return result
end

function hub.configureDesktop()
  local root = dom.div {style = "padding:1em;background:Canvas;color:CanvasText"}
  local server = dom.input {type = "url", placeholder = "https://seu-silverbullet", style = "width:100%;padding:.6em"}
  local input = dom.input {type = "password", autocomplete = "off", ["aria-label"] = "Token SilverBullet deste computador",
    style = "width:100%;padding:.6em;box-sizing:border-box"}
  local status = dom.p {role = "status", ["aria-live"] = "polite"}
  root.appendChild(dom.h3 {__rawText = "Conectar integrações ao servidor"})
  root.appendChild(dom.p {__rawText = "Crie um token da sua conta no Dashboard do servidor. Use um token diferente para cada computador. Não use as chaves Zotero/Vikunja/Readeck."})
  root.appendChild(dom.p {__rawText = "O token ficará no clientStore local, fora das notas e do Git. Não é protegido pelo Keychain; qualquer script autorizado neste Space pode lê-lo."})
  root.appendChild(server); root.appendChild(input); root.appendChild(status)
  local busy = false
  root.appendChild(dom.button {__rawText = "Testar e salvar neste computador", onclick = function()
    if busy then return end
    local token = tostring(input.value or "")
    token = token:gsub("^%s+", "")
    token = token:gsub("%s+$", "")
    if #token < 16 then status.textContent = "Cole um token SilverBullet válido."; return end
    local address = tostring(server.value or ""):gsub("/+$", "")
    if not address:match("^https://[%w%.%-:]+$") then status.textContent = "Use a origem HTTPS do seu SilverBullet."; return end
    busy = true; status.textContent = "Verificando conexão privada…"
    local previous = hub.tailnet
    hub.connectingServer = address
    local ok, result = pcall(hub.remoteCall, "status", "GET", nil, token)
    hub.connectingServer = nil
    busy = false
    if not ok or result.status ~= 200 or type(result.body) ~= "table" or not result.body.jobs then
      status.textContent = "Não foi possível conectar. Verifique Tailscale, token e acesso write ao Space."
      return
    end
    clientStore.set("personal-hub.desktop.v1", {token = token, server = address})
    input.value = ""
    editor.hidePanel("modal")
    editor.flashNotification("Integrações conectadas ao servidor neste computador.", "info")
  end})
  root.appendChild(dom.button {__rawText = "Cancelar", onclick = function() input.value = ""; editor.hidePanel("modal") end})
  editor.showPanel("modal", "5vh 3vw", root)
end

function hub.waitLocalPage(page)
  if not hub.desktopConnection() then return end
  for attempt = 1, 40 do
    if space.pageExists(page) then return end
    -- Wait for Desktop's native sync. Never copy over an unsynced local page.
    js.new(js.window.Promise, function(resolve) js.window.setTimeout(resolve, 250) end)
  end
  error("Nota criada no servidor, mas ainda não sincronizada neste Desktop. Verifique o Sync e selecione novamente; nenhuma nota local foi sobrescrita.")
end

function hub.navigate(page)
  hub.waitLocalPage(page)
  editor.navigate(page)
end

function hub.toJSON(value)
  return js.window.JSON.stringify(js.tojs(value), nil, 2)
end

function hub.fromJSON(value)
  return js.tolua(js.window.JSON.parse(value))
end

function hub.statusWidget()
  local root = dom.div {}
  local ok, data = pcall(hub.call, "status")
  if not ok then return widget.htmlBlock(dom.p {__rawText = tostring(data)}) end
  for _, name in ipairs({"daily", "zotero", "readeck", "google_calendar"}) do
    local job = data.jobs[name] or {}
    root.appendChild(dom.p {__rawText = name .. ": " .. tostring(job.error or "sem erro registrado") ..
      " · último sucesso: " .. tostring(job.last_success or "nenhum")})
  end
  root.appendChild(dom.p {__rawText = "Git: " .. tostring(data.git.error or data.git.last_push or "sem envio registrado")})
  root.appendChild(dom.p {__rawText = "Tarefas com criação incerta: " .. tostring(data.uncertain_tasks or 0)})
  root.appendChild(dom.button {__rawText = "Atualizar estado", onclick = function() editor.navigate(editor.getCurrentPage()) end})
  return widget.htmlBlock(root)
end

function hub.call(path, method, body)
  local options = {method = method or "GET"}
  if body then options.body = body; options.headers = {["Content-Type"] = "application/json"} end
  local connection = hub.desktopConnection()
  local result
  if connection then
    result = hub.remoteCall(path, method, body, connection.token)
  else
    result = net.proxyFetch("http://127.0.0.1:8088/v1/" .. path, options)
  end
  if result.status < 200 or result.status >= 300 then
    local message = "Integração: HTTP " .. tostring(result.status)
    if type(result.body) == "table" and result.body.detail then message = result.body.detail end
    error(message)
  end
  return result.body
end

function hub.select(label, rows, format)
  local options = {}
  for _, row in ipairs(rows) do
    table.insert(options, {name = format(row), value = row})
  end
  local selected = editor.filterBox(label, options)
  if selected then return selected.value end
end

function hub.liveSearch(source, onSelect)
  local root = dom.div {style = "padding:1em;background:var(--modal-background-color, Canvas);color:var(--root-color, CanvasText);max-height:75vh;overflow:auto"}
  local input = dom.input {type = "search", placeholder = "Digite parte do autor ou título…", ["aria-label"] = "Buscar " .. source,
    style = "width:100%;box-sizing:border-box;padding:.7em;font-size:1em"}
  local status = dom.p {__rawText = "Carregando itens…", role = "status", ["aria-live"] = "polite"}
  local list = dom.div {}
  local version, timer, busy, closed = 0, nil, false, false
  local buttons = {}
  local cachedRows = {}
  local function close()
    closed = true
    if timer then js.window.clearTimeout(timer) end
    editor.hidePanel("modal")
  end
  local function choose(row)
    close()
    local ok, err = pcall(onSelect, row)
    if not ok then editor.flashNotification(tostring(err), "error") end
  end
  local function renderRows(rows)
    list.replaceChildren()
    buttons = {}
    for index, row in ipairs(rows) do
      local item, position = row, index
      local label = item.title or "Sem título"
      if source == "zotero" then label = table.concat(item.authors or {}, ", ") .. " · " .. tostring(item.year or "s.d.") .. " · " .. label end
      local button = dom.button {__rawText = label, type = "button",
        style = "display:block;width:100%;text-align:left;padding:.65em;margin:.25em 0;white-space:normal",
        onclick = function() choose(item) end,
        onkeydown = function(event)
          if event.key == "ArrowDown" and buttons[position + 1] then event.preventDefault(); buttons[position + 1].focus() end
          if event.key == "ArrowUp" then event.preventDefault(); if position == 1 then input.focus() else buttons[position - 1].focus() end end
        end}
      table.insert(buttons, button)
      list.appendChild(button)
    end
  end
  local load
  load = function()
    if closed or busy or not root.isConnected then return end
    local term = tostring(input.value or "")
    if #term == 1 then status.textContent = "Digite pelo menos 2 caracteres."; renderRows({}); return end
    busy = true
    local current = version
    status.textContent = "Buscando mais resultados…"
    local ok, rows = pcall(hub.call, source .. "/search?q=" .. js.window.encodeURIComponent(term))
    busy = false
    if closed or not root.isConnected then return end
    if current ~= version then
      if timer then js.window.clearTimeout(timer); timer = nil end
      load(); return
    end
    if not ok then status.textContent = tostring(rows); return end
    cachedRows = rows
    renderRows(rows)
    status.textContent = tostring(#rows) .. " resultado(s) · até 50 itens · busca por autor/título"
    if #rows == 0 then status.textContent = "Nenhum resultado. Tente parte do título ou outro autor." end
  end
  input.addEventListener("input", function()
    version = version + 1
    if timer then js.window.clearTimeout(timer) end
    local term = tostring(input.value or ""):lower()
    local matches = {}
    if #term ~= 1 then
      for _, row in ipairs(cachedRows) do
        local text = (row.title or "") .. " " .. table.concat(row.authors or {}, " ") .. " " .. tostring(row.year or "")
        if term == "" or text:lower():find(term, 1, true) then table.insert(matches, row) end
      end
    end
    renderRows(matches)
    status.textContent = tostring(#matches) .. " match(es) já carregados · aguardando busca na biblioteca…"
    if #term == 1 then status.textContent = "Digite pelo menos 2 caracteres." end
    timer = js.window.setTimeout(load, 350)
  end)
  input.addEventListener("keydown", function(event)
    if event.key == "ArrowDown" and buttons[1] then event.preventDefault(); buttons[1].focus() end
    if event.key == "Enter" and buttons[1] then event.preventDefault(); buttons[1].click() end
  end)
  root.addEventListener("keydown", function(event) if event.key == "Escape" then event.preventDefault(); close() end end)
  root.appendChild(dom.h3 {__rawText = "Buscar " .. source})
  root.appendChild(input); root.appendChild(status); root.appendChild(list)
  root.appendChild(dom.button {__rawText = "Fechar", onclick = close})
  editor.showPanel("modal", "5vh 3vw", root)
  local mounted
  local attempts = 0
  mounted = function()
    if closed then return end
    attempts = attempts + 1
    if not root.isConnected then
      if attempts < 20 then js.window.setTimeout(mounted, 50) end
      return
    end
    input.focus(); load()
  end
  js.window.setTimeout(mounted, 50)
end

function hub.zoteroSelected(item) return paperwork.zoteroSelected(item) end
function hub.zotero() return paperwork.zotero() end

function hub.readeckSelected(item)
  local action = editor.filterBox("Bookmark", {
    {name = "Inserir link na nota atual", value = "link"},
    {name = "Importar/atualizar e abrir nota com highlights", value = "import"}
  })
  if not action then return end
  if action.value == "import" then
    local result = hub.call("readeck/bookmarks/" .. item.id .. "/import", "POST")
    hub.navigate(result.path)
  elseif item.path then
    editor.insertAtCursor("[[" .. item.path:gsub("%.md$", "") .. "]]")
  else
    editor.insertAtCursor(item.url)
  end
end

function hub.readeck()
  hub.liveSearch("readeck", hub.readeckSelected)
end

function hub.editTask(task)
  local field = editor.filterBox("Editar tarefa #" .. tostring(task.id), {
    {name = "Título", value = "title"}, {name = "Prazo", value = "due_date"},
    {name = "Prioridade", value = "priority"}, {name = "Projeto", value = "project_id"},
    {name = "Labels", value = "labels"}
  })
  if not field then return end
  local patch = {}
  if field.value == "project_id" then
    local project = hub.select("Projeto", hub.call("projects"), function(p) return tostring(p.id) .. " · " .. p.title end)
    if not project then return end
    patch.project_id = project.id
  elseif field.value == "labels" then
    local list = hub.call("labels")
    local help = "IDs separados por vírgula (vazio remove):\n"
    for _, label in ipairs(list) do help = help .. tostring(label.id) .. " · " .. label.title .. "\n" end
    local value = editor.prompt(help, "")
    if value == nil then return end
    patch.labels = {}
    for id in value:gmatch("%d+") do table.insert(patch.labels, tonumber(id)) end
  else
    local value = editor.prompt(field.value .. " (prazo YYYY-MM-DD; prioridade 0–5)", tostring(task[field.value] or ""))
    if value == nil then return end
    if field.value == "priority" then value = tonumber(value) end
    if field.value == "due_date" and value == "" then value = "0001-01-01T00:00:00Z" end
    patch[field.value] = value
  end
  hub.call("tasks/" .. tostring(task.id), "PATCH", patch)
  editor.flashNotification("Atualizado no Vikunja.", "info")
end

function hub.taskView(kind, afterDialog, forceInitial)
  local root = dom.div {class = "personal-task-dashboard"}
  local function refresh(force)
    root.replaceChildren()
    local path = kind == "today" and "tasks/today" or "tasks/saved-filter"
    if force and kind ~= "today" then path = path .. "?force=true" end
    local ok, data = pcall(hub.call, path)
    if not ok then root.appendChild(dom.p {__rawText = tostring(data)}); return end
    root.appendChild(dom.p {__rawText = (data.title or "Tarefas do dia") .. " · " .. data.updated})
    root.appendChild(dom.button {__rawText = "Atualizar", onclick = function() refresh(true) end})
    for _, task in ipairs(data.tasks) do
      local current = task
      local row = dom.div {style = "display:flex;gap:.6em;align-items:center;margin:.7em 0;flex-wrap:wrap"}
      row.appendChild(dom.button {__rawText = current.done and "Reabrir" or "Concluir", onclick = function()
        hub.call("tasks/" .. tostring(current.id), "PATCH", {done = not current.done}); refresh(true)
      end})
      row.appendChild(dom.span {__rawText = "#" .. tostring(current.id) .. " · " .. current.title})
      row.appendChild(dom.button {__rawText = "Editar", onclick = function()
        hub.editTask(current); refresh(true)
        if afterDialog then afterDialog() end
      end})
      row.appendChild(dom.button {__rawText = "Detalhes", onclick = function()
        local full = hub.call("tasks/" .. tostring(current.id))
        editor.alert("#" .. tostring(full.id) .. " · " .. full.title .. "\nPrazo: " .. tostring(full.due_date) ..
          "\nPrioridade: " .. tostring(full.priority) .. "\nProjeto: " .. tostring(full.project_id))
        if afterDialog then afterDialog() end
      end})
      root.appendChild(row)
    end
  end
  refresh(forceInitial or false)
  local timer
  timer = js.window.setInterval(function()
    if not root.isConnected then js.window.clearInterval(timer); return end
    if not js.window.document.hidden then refresh(false) end
  end, 60000)
  return root
end

function hub.taskWidget(kind)
  return widget.htmlBlock(hub.taskView(kind))
end

function hub.taskPopup(kind)
  local root = dom.div {style = "padding:1em;background:Canvas;color:CanvasText;max-height:75vh;overflow:auto"}
  local helpers = js.importFromSpace("Library/Relay/Scripts/Panels.js")
  helpers.isolateKeyboard(root)
  root.appendChild(dom.h2 {__rawText = kind == "today" and "Tarefas de hoje · Vikunja" or "Todas as Tarefas · Vikunja"})
  local close = dom.button {__rawText = "Fechar (Esc)", onclick = function() editor.hidePanel("modal") end}
  root.appendChild(close)
  helpers.dismissOnEscape(root, close)
  local function show()
    editor.showPanel("modal", "10vh max(16px, calc((100vw - 760px) / 2))", root)
    js.window.setTimeout(function() if root.isConnected then close.focus() end end, 50)
  end
  root.appendChild(hub.taskView(kind, show, true))
  show()
end

function hub.daily() return paperwork.daily() end
function hub.dailyButton() return paperwork.dailyButton() end

function hub.taskProject()
  local line = editor.getCurrentLine()
  if not line.text:match("%[[ xX]%]") then error("Posicione o cursor em uma tarefa da Daily.") end
  local project = hub.select("Projeto (padrão Inbox)", hub.call("projects"), function(p) return tostring(p.id) .. " · " .. p.title end)
  if not project then return end
  local marker = line.text:match("<!%-%- sb%-task:(.-) %-%->")
  local meta = marker and hub.fromJSON(marker) or {origin = tostring(js.window.crypto.randomUUID())}
  meta.project = project.id
  local text = line.text:gsub("%s*<!%-%- sb%-task:.- %-%->", "") .. " <!-- sb-task:" .. js.window.JSON.stringify(js.tojs(meta)) .. " -->"
  editor.replaceRange(line.from, line.to, text)
  editor.save()
end

command.define {name = "Integrações: Conectar Desktop ao servidor", run = hub.configureDesktop}
command.define {name = "Integrações: Conectar Desktop pela Tailscale", run = hub.configureDesktop}
command.define {name = "Integrações: Desconectar este computador", run = function()
  clientStore.delete("personal-hub.desktop.v1")
  editor.flashNotification("Conexão local removida. Revogue também o token no Dashboard do servidor.", "info")
end}
command.define {name = "Readeck: Buscar bookmarks", run = hub.readeck}
command.define {name = "Vikunja: Projeto da tarefa atual", run = hub.taskProject}
function hub.syncDaily() return paperwork.syncDaily() end
command.define {name = "Vikunja: Todas tarefas", run = function() hub.taskPopup("all") end}
command.define {name = "Vikunja: Tarefas de hoje", run = function() hub.taskPopup("today") end}
slashCommand.define {name = "readeck", description = "Buscar bookmark ou link interno", run = hub.readeck}

```
