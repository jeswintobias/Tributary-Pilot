// Just enough XML, written by hand.
//
// No dependency, because the alternative is pulling a builder into a service
// whose entire job is producing four message shapes, and because escaping is
// the only hard part and it is twelve lines.
//
// Escaping is not optional here. A beneficiary reference is a salted hash
// today, but the same field will one day carry a name, and a name with an
// ampersand in it must not be able to produce a document that parses as
// something else.

const ESCAPES = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&apos;' };

export function esc(value) {
  if (value === null || value === undefined) return '';
  return String(value).replace(/[&<>"']/g, (c) => ESCAPES[c]);
}

/**
 * One element. `attrs` values are escaped; so is text.
 *
 * Children are passed already-rendered, which keeps the builders below
 * readable as the message structure rather than as function calls.
 */
export function el(name, children, attrs = {}) {
  const rendered = Object.entries(attrs)
    .filter(([, v]) => v !== undefined && v !== null && v !== '')
    .map(([k, v]) => ` ${k}="${esc(v)}"`)
    .join('');
  if (children === null || children === undefined || children === '') {
    return `<${name}${rendered}/>`;
  }
  return `<${name}${rendered}>${children}</${name}>`;
}

/** An element whose content is text, escaped. */
export function text(name, value, attrs = {}) {
  return el(name, esc(value), attrs);
}

export function indent(xml) {
  let depth = 0;
  return xml
    .replace(/></g, '>\n<')
    .split('\n')
    .map((line) => {
      if (/^<\//.test(line)) depth -= 1;
      const out = '  '.repeat(Math.max(0, depth)) + line;
      if (/^<[^/?!][^>]*[^/]>$/.test(line) && !/^<.*<\/.*>$/.test(line)) depth += 1;
      return out;
    })
    .join('\n');
}

export const DECLARATION = '<?xml version="1.0" encoding="UTF-8"?>';
