// Pure helpers for the OCPP debug console: a tolerant JSON tokenizer for
// syntax highlighting, caret-context analysis for field suggestions, JSON
// error positions and schema warnings against the backend's field hints.

export interface ConsoleField {
  path: string; // dotted; array elements as "name[]", e.g. "a.b[].c"
  type: string; // integer | number | boolean | string | datetime | array | object
  required?: boolean;
  enum?: string[];
}

export interface ConsoleAction {
  action: string;
  profile: string;
  template: unknown;
  fields: ConsoleField[];
}

export type TokenType = 'key' | 'string' | 'number' | 'boolean' | 'null' | 'punct' | 'space' | 'plain';
export interface Token { type: TokenType; text: string }

// tokenize splits (possibly invalid) JSON text into highlightable tokens.
// Concatenating every token's text always reproduces the input exactly.
export function tokenize(text: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < text.length) {
    const c = text[i];
    if (c === '"') {
      let j = i + 1;
      while (j < text.length && text[j] !== '"' && text[j] !== '\n') j += text[j] === '\\' ? 2 : 1;
      if (text[j] === '"') j++;
      j = Math.min(j, text.length);
      // A string followed by a colon is an object key.
      let k = j;
      while (k < text.length && /[ \t\r\n]/.test(text[k])) k++;
      tokens.push({ type: text[k] === ':' ? 'key' : 'string', text: text.slice(i, j) });
      i = j;
    } else if (/\s/.test(c)) {
      let j = i;
      while (j < text.length && /\s/.test(text[j])) j++;
      tokens.push({ type: 'space', text: text.slice(i, j) });
      i = j;
    } else if ('{}[],:'.includes(c)) {
      tokens.push({ type: 'punct', text: c });
      i++;
    } else {
      const m = /^-?\d+(\.\d+)?([eE][+-]?\d+)?/.exec(text.slice(i))
        ?? /^(true|false|null)\b/.exec(text.slice(i));
      if (m) {
        const word = m[0];
        tokens.push({ type: word === 'null' ? 'null' : word === 'true' || word === 'false' ? 'boolean' : 'number', text: word });
        i += word.length;
      } else {
        let j = i + 1;
        while (j < text.length && !/[\s"{}[\],:]/.test(text[j])) j++;
        tokens.push({ type: 'plain', text: text.slice(i, j) });
        i = j;
      }
    }
  }
  return tokens;
}

// jsonError returns null for valid JSON, else the parser message with a
// 1-based line and column.
export function jsonError(text: string): { message: string; line: number; column: number } | null {
  try {
    JSON.parse(text);
    return null;
  } catch (e) {
    const message = (e as Error).message;
    const lc = /line (\d+) column (\d+)/.exec(message);
    if (lc) return { message: message.replace(/\s*\(line \d+ column \d+\)/, ''), line: +lc[1], column: +lc[2] };
    const pos = /position (\d+)/.exec(message);
    const offset = pos ? +pos[1] : text.length;
    const before = text.slice(0, offset).split('\n');
    return { message, line: before.length, column: before[before.length - 1].length + 1 };
  }
}

export const parentPath = (path: string) => {
  const dot = path.lastIndexOf('.');
  return dot < 0 ? '' : path.slice(0, dot);
};
export const leafName = (path: string) => path.slice(path.lastIndexOf('.') + 1);

export type Warning =
  | { kind: 'unknown'; path: string }
  | { kind: 'missing'; path: string }
  | { kind: 'enum'; path: string; values: string[] }
  | { kind: 'type'; path: string; type: string };

function typeMatches(type: string, value: unknown): boolean {
  switch (type) {
    case 'integer': return Number.isInteger(value);
    case 'number': return typeof value === 'number';
    case 'boolean': return typeof value === 'boolean';
    case 'string': return typeof value === 'string';
    case 'datetime': return typeof value === 'string' && !Number.isNaN(Date.parse(value));
    case 'array': return Array.isArray(value);
    case 'object': return typeof value === 'object' && value !== null && !Array.isArray(value);
    default: return true;
  }
}

// validatePayload checks a parsed payload against the action's field hints.
export function validatePayload(value: unknown, fields: ConsoleField[]): Warning[] {
  const byPath = new Map(fields.map((f) => [f.path, f]));
  const warnings: Warning[] = [];
  const walkObject = (obj: unknown, path: string) => {
    if (typeof obj !== 'object' || obj === null || Array.isArray(obj)) return;
    const record = obj as Record<string, unknown>;
    for (const f of fields) {
      if (f.required && parentPath(f.path) === path && !(leafName(f.path) in record)) warnings.push({ kind: 'missing', path: f.path });
    }
    for (const [key, child] of Object.entries(record)) {
      const childPath = path ? `${path}.${key}` : key;
      const field = byPath.get(childPath);
      if (!field) { warnings.push({ kind: 'unknown', path: childPath }); continue; }
      if (!typeMatches(field.type, child)) { warnings.push({ kind: 'type', path: childPath, type: field.type }); continue; }
      if (field.enum?.length && typeof child === 'string' && !field.enum.includes(child)) {
        warnings.push({ kind: 'enum', path: childPath, values: field.enum });
      }
      if (field.type === 'object') walkObject(child, childPath);
      if (field.type === 'array' && Array.isArray(child)) {
        for (const item of child) walkObject(item, `${childPath}[]`);
      }
    }
  };
  walkObject(value, '');
  return warnings;
}

export interface CaretContext {
  mode: 'key' | 'value';
  path: string; // path of the object the caret is in ('' = root)
  key?: string; // for value mode: the property being edited
  partial: string; // typed prefix
  replaceFrom: number; // offset where the typed token (incl. opening quote) starts
  existing: string[]; // keys already present before the caret
}

interface Frame { type: 'obj' | 'arr'; key?: string; expectKey: boolean; lastKey?: string; keys: string[] }

// caretContext works out whether the caret sits where an object key or a
// value is typed, and under which field path, for completion suggestions.
export function caretContext(text: string, caret: number): CaretContext | null {
  const stack: Frame[] = [];
  let strStart = -1;
  let strIsKey = false;
  let pendingKey: string | undefined;
  for (let i = 0; i < caret; i++) {
    const c = text[i];
    const top = stack[stack.length - 1];
    if (strStart >= 0) {
      if (c === '\\') { i++; continue; }
      if (c === '"') {
        const s = text.slice(strStart + 1, i);
        if (strIsKey && top) { pendingKey = s; top.keys.push(s); }
        strStart = -1;
      }
      continue;
    }
    if (c === '"') { strStart = i; strIsKey = !!top && top.type === 'obj' && top.expectKey; continue; }
    if (c === '{' || c === '[') {
      const key = !top ? undefined : top.type === 'arr' ? '[]' : top.lastKey;
      stack.push({ type: c === '{' ? 'obj' : 'arr', key, expectKey: c === '{', keys: [] });
    } else if (c === '}' || c === ']') {
      stack.pop();
    } else if (c === ':' && top?.type === 'obj') {
      top.expectKey = false;
      top.lastKey = pendingKey;
    } else if (c === ',' && top?.type === 'obj') {
      top.expectKey = true;
      top.lastKey = undefined;
    }
  }
  const top = stack[stack.length - 1];
  if (!top || top.type !== 'obj') return null;
  const segments: string[] = [];
  for (const frame of stack.slice(1)) {
    if (frame.key === '[]') {
      if (segments.length) segments[segments.length - 1] += '[]';
    } else segments.push(frame.key ?? '?');
  }
  const path = segments.join('.');
  let partial: string;
  let replaceFrom: number;
  if (strStart >= 0) {
    partial = text.slice(strStart + 1, caret);
    replaceFrom = strStart;
  } else {
    const m = /[A-Za-z0-9_]*$/.exec(text.slice(0, caret))!;
    partial = m[0];
    replaceFrom = caret - partial.length;
    // Only right after "{" / "," (key) or ":" (value); not after a finished value.
    const before = text.slice(0, replaceFrom).trimEnd();
    if (top.expectKey ? !/[{,]$/.test(before) : !before.endsWith(':')) return null;
  }
  if (top.expectKey) return { mode: 'key', path, partial, replaceFrom, existing: top.keys };
  return { mode: 'value', path, key: top.lastKey, partial, replaceFrom, existing: top.keys };
}

// childFields lists the fields directly inside the object at `path`.
export function childFields(fields: ConsoleField[], path: string): ConsoleField[] {
  return fields.filter((f) => parentPath(f.path) === path);
}

// placeholderFor returns the value inserted after a completed key, and how
// far back from its end the caret should land.
export function placeholderFor(field: ConsoleField): { text: string; caretBack: number } {
  switch (field.type) {
    case 'string': case 'datetime': return { text: '""', caretBack: 1 };
    case 'integer': case 'number': return { text: '0', caretBack: 0 };
    case 'boolean': return { text: 'false', caretBack: 0 };
    case 'array': return { text: '[]', caretBack: 1 };
    case 'object': return { text: '{}', caretBack: 1 };
    default: return { text: 'null', caretBack: 0 };
  }
}
