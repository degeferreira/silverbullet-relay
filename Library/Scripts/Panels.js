export function isolateKeyboard(element) {
  // Shadow DOM retargets events to the panel host. Global editor shortcuts then
  // miss the focused textarea and prevent native Backspace/undo/etc. A direct
  // synchronous JS listener stops bubbling before those handlers run; an async
  // Space Lua callback can be too late. Never prevent the field's default action.
  for (const type of ["keydown", "keyup"]) {
    element.addEventListener(type, event => event.stopPropagation());
  }
}
export function dismissOnEscape(element, button) {
  element.addEventListener("keydown", event => {
    if (event.key !== "Escape" || event.isComposing || event.keyCode === 229) return;
    event.preventDefault();
    event.stopPropagation();
    if (!event.repeat) return button.click();
  });
}
