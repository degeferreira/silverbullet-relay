---
tags: meta
---

```space-lua
-- priority: 3
function hub.settings()
  local ok, values = pcall(hub.call, "settings")
  if not ok then editor.flashNotification(tostring(values), "error"); return end
  local root = dom.div {style = "padding:1em;max-height:80vh;overflow:auto;background:Canvas;color:CanvasText"}
  root.appendChild(dom.h2 {__rawText = "Relay — Configurações do servidor"})
  local inputs = {}
  for _, spec in ipairs({
    {"timezone", "Fuso horário"}, {"vikunja_url", "Vikunja — URL da API acessível ao servidor"},
    {"vikunja_public_url", "Vikunja — URL para abrir no navegador"},
    {"inbox_id", "Vikunja — ID do projeto Inbox"}, {"saved_filter_id", "Vikunja — ID do filtro salvo"},
    {"readeck_url", "Readeck — URL da API"}, {"zotero_user_id", "Zotero — User ID (opcional)"},
    {"daily_interval", "Intervalo Daily (segundos)"}, {"zotero_interval", "Intervalo Zotero (segundos)"},
    {"readeck_interval", "Intervalo Readeck (segundos)"}, {"readeck_trigger_labels", "Labels de importação Readeck (separadas por vírgula)"},
    {"readeck_processed_label", "Label Readeck após importar"}, {"google_calendar_interval", "Intervalo Google Calendar (segundos)"}
  }) do
    local field = dom.input {value = tostring(values[spec[1]] or ""), style = "display:block;width:95%;padding:.4em"}
    local label = dom.label {__rawText = spec[2]}
    label.appendChild(field); root.appendChild(label); inputs[spec[1]] = field
  end
  for _, name in ipairs({"daily", "zotero", "readeck"}) do
    local field = dom.input {type = "checkbox", checked = values[name .. "_enabled"]}
    local label = dom.label {__rawText = "Ativar sincronização: " .. name}
    label.appendChild(field); root.appendChild(label); inputs[name .. "_enabled"] = field
  end
  local calendar = dom.input {type = "checkbox", checked = values.google_calendar_enabled}
  local calendarLabel = dom.label {__rawText = "Ativar calendário dedicado (requer OAuth configurado no servidor)"}
  calendarLabel.appendChild(calendar); root.appendChild(calendarLabel)
  inputs.google_calendar_enabled = calendar
  local status = dom.p {role = "status"}
  root.appendChild(dom.p {__rawText = "Credenciais ficam no servidor, fora das notas e do Git. Alterações exigem reinício do serviço. Para outra instância Vikunja, use um estado novo; não reutilize os mapeamentos antigos."})
  root.appendChild(dom.button {__rawText = "Salvar configuração", onclick = function()
    local body = {}
    for name, field in pairs(inputs) do
      if name:match("_enabled$") then body[name] = field.checked else body[name] = tostring(field.value or "") end
    end
    body.inbox_id = tonumber(body.inbox_id); body.saved_filter_id = tonumber(body.saved_filter_id)
    body.zotero_user_id = tonumber(body.zotero_user_id)
    for _, name in ipairs({"daily_interval", "zotero_interval", "readeck_interval", "google_calendar_interval"}) do body[name] = tonumber(body[name]) end
    local success, result = pcall(hub.call, "settings", "PUT", body)
    status.textContent = success and "Salvo. Reinicie o serviço de integração para aplicar." or tostring(result)
  end})
  for _, credential in ipairs({{"vikunja_token", "Vikunja"}, {"zotero_key", "Zotero"}, {"readeck_token", "Readeck"}}) do
    local spec = credential
    local field = dom.input {type = "password", autocomplete = "off", placeholder = spec[2] .. " — nova credencial"}
    root.appendChild(field)
    root.appendChild(dom.button {__rawText = "Salvar credencial " .. spec[2], onclick = function()
      local ok, result = pcall(hub.call, "settings/secrets/" .. spec[1], "PUT", {value = tostring(field.value or "")})
      if ok then field.value = "" end
      status.textContent = ok and "Credencial salva. Reinicie o serviço para aplicar." or tostring(result)
    end})
  end
  root.appendChild(status)
  root.appendChild(dom.button {__rawText = "Fechar", onclick = function() editor.hidePanel("modal") end})
  editor.showPanel("modal", "5vh 3vw", root)
end
command.define {name = "Relay: Configurações", run = hub.settings}
```
