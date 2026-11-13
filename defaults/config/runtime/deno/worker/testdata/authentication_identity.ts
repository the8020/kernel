// Runtime regression fixture: approves the subject without a database.
export function authenticate(
  _request: Request,
  input: { sub: string },
): { userId: string; username: string } {
  return { userId: input.sub, username: input.sub.slice(5) };
}
