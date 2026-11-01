// Generated JavaScript: edit this TypeScript source and run npm run build.
// Pure text transform: never rewrites manually edited references.
export function addReference(text: string, identity: string, reference: string, path: string) {
  if (!/^user:\d+:[A-Z0-9]{8}$/.test(identity)) throw new Error("Identidade Zotero inválida");
  const start = `<!-- APA:${identity}:START -->`;
  const end = `<!-- APA:${identity}:END -->`;
  if (text.includes(start)) {
    if (text.split(start).length !== 2 || text.split(end).length !== 2 || text.indexOf(start) >= text.indexOf(end)) {
      throw new Error("Marcadores APA ambíguos; a nota foi preservada.");
    }
    // Stable ID deduplication; a user can edit the existing reference safely.
    return text;
  }
  if (text.includes(end)) throw new Error("Marcadores APA incompletos; a nota foi preservada.");
  const headings = [];
  const otherHeadings = [];
  let offset = 0, fence: string | null = null, frontmatter = text.startsWith("---\n") || text.startsWith("---\r\n");
  for (const line of text.split(/(?<=\n)/)) {
    const plain = line.replace(/\r?\n$/, "");
    if (frontmatter) {
      if (offset && plain === "---") frontmatter = false;
    } else {
      const delimiter = /^\s{0,3}(`{3,}|~{3,})/.exec(plain);
      if (delimiter) {
        if (!fence) fence = delimiter[1];
        else if (delimiter[1][0] === fence[0] && delimiter[1].length >= fence.length) fence = null;
      } else if (!fence && /^#{1,6}[ \t]+/.test(plain)) {
        otherHeadings.push(offset);
        if (/^#{1,6}[ \t]+Referências[ \t]*$/.test(plain)) headings.push({index: offset, length: plain.length});
      }
    }
    offset += line.length;
  }
  if (headings.length > 1) throw new Error("Há mais de uma seção Referências; escolha uma antes de citar.");
  const noteLink = path ? "\n\n" + internalCitation(path, "Notas e highlights") : "";
  const entry = `${start}\n${reference}${noteLink}\n${end}\n`;
  const gap = (value: string) => value.endsWith("\n\n") ? "" : value.endsWith("\n") ? "\n" : "\n\n";
  if (!headings.length) return text + gap(text) + "## Referências\n\n" + entry;
  const heading = headings[0];
  const endOffset = otherHeadings.find(position => position > heading.index) ?? text.length;
  const before = text.slice(0, endOffset);
  return before + gap(before) + entry + text.slice(endOffset);
}

export function internalCitation(path: string, citation: string) {
  if (/[\]\[|\r\n]/.test(path)) throw new Error("Caminho de referência inválido");
  // SilverBullet's [[page|alias]] renders the APA label, not the item key.
  return `[[${path}|${citation.replace(/[\]\[|\r\n]/g, " ")}]]`;
}
