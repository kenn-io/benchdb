// Characters a POSIX shell never treats specially in an unquoted word.
const SHELL_SAFE = /^[A-Za-z0-9@%+=:,./_-]+$/;

/** shellQuote makes a value safe to paste as one POSIX shell word. Safe values
 * stay bare so common commands read naturally; anything else is wrapped in
 * single quotes, with embedded single quotes closed, escaped, and reopened. */
export function shellQuote(value: string): string {
  if (SHELL_SAFE.test(value)) return value;
  return `'${value.replaceAll("'", `'\\''`)}'`;
}
