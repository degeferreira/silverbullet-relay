---
tags: meta
---

# Notas bibliográficas

Use **Zotero: Buscar biblioteca** → **Gerar ou atualizar nota**. A busca atualiza os resultados enquanto você digita (350 ms de pausa, pelo menos dois caracteres). O campo vazio mostra os primeiros itens.

Ordem da nota: referência completa APA, tags do Zotero, suas tags, abstract/resumo, notas/highlights importados, suas notas e mais coisas. Os trechos entre marcadores `ZOTERO` são atualizados automaticamente; escreva em **Minhas tags**, **Minhas notas** e **Mais coisas**. As tags pessoais adicionadas ao frontmatter também são preservadas.

O abstract disponível no Zotero é importado sem tradução automática. Para priorizar uma versão portuguesa, use `Abstract[pt-BR]: resumo...` no campo Extra do Zotero, ou `abstractPt: resumo...` no frontmatter da nota. Sem isso, utiliza-se o abstract original, normalmente inglês.

Use `/cite`, escolha o item e **Citar em APA**: insere um link com rótulo autor–ano, cria a nota bibliográfica se necessário e adiciona a referência completa a **Referências** na nota atual. A identidade interna continua sendo o ID da biblioteca + item key. Repetir a citação não duplica a entrada bibliográfica; referências editadas manualmente não são sobrescritas. CSL JSON, BibLaTeX e citekeys continuam disponíveis em `Bibliography/` para exportações.
