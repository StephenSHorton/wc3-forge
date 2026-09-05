// Shared "is the user typing?" gate for window/document keydown hotkeys.
//
// A tagName check for <input>/<textarea> is not enough. Monaco 0.52+ uses
// Chromium's EditContext API when it's available (WebView2 included): the
// focused node is a DIV inside `.monaco-editor`, not a <textarea>. The
// Frame Selection hotkey (`f`) used to see that DIV, call preventDefault,
// and swallow the character — so typing `function` in a custom script
// produced `unction` (github.com/StephenSHorton/wc3-forge/issues/34).
//
// Callers should pass e.target; we also inspect document.activeElement so
// a retargeted keydown (dialog focus trap, capture-phase listener) still
// counts as typing when the editor actually has focus.

const TYPING_TAGS = new Set(['INPUT', 'TEXTAREA', 'SELECT'])

export function isTypingTarget(target?: EventTarget | null): boolean {
  return isTypingElement(asHTMLElement(target))
    || isTypingElement(asHTMLElement(document.activeElement))
}

function asHTMLElement(t: EventTarget | null | undefined): HTMLElement | null {
  if (!t) return null
  if (t instanceof HTMLElement) return t
  if (t instanceof Element && t.parentElement) return t.parentElement
  return null
}

function isTypingElement(el: HTMLElement | null): boolean {
  if (!el) return false
  if (el.isContentEditable) return true
  if (TYPING_TAGS.has(el.tagName)) return true
  // Monaco (textarea *or* EditContext) and anything advertising itself as
  // a textbox. closest() matches the element itself, so a focused
  // `.monaco-editor` host is covered.
  if (el.closest('.monaco-editor, .monaco-diff-editor, .native-edit-context, [role="textbox"]')) return true
  return false
}
