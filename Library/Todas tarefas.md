---
tags: meta
---

# TODAS TAREFAS

Consulta dinâmica ao filtro salvo do Vikunja. Os dados não são copiados para este Markdown.

${(function() local ok, result = pcall(function() return hub.taskWidget("all") end); if ok then return result end; return "Diagnóstico: hub=" .. type(hub) .. ", widget=" .. type(widget) .. ", dom=" .. type(dom) .. ", command=" .. type(command) .. ", net=" .. type(net) .. ". Erro: " .. tostring(result) end)()}
