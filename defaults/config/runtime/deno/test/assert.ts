export function assertEquals(actual: unknown, expected: unknown): void {
  const actualJSON = JSON.stringify(actual);
  const expectedJSON = JSON.stringify(expected);
  if (actualJSON !== expectedJSON) {
    throw new Error(`assertEquals failed: ${actualJSON} !== ${expectedJSON}`);
  }
}

export async function assertRejects(
  operation: () => Promise<unknown>,
  constructor: typeof Error,
  includes: string,
): Promise<void> {
  try {
    await operation();
  } catch (error) {
    if (!(error instanceof constructor)) {
      throw new Error(`unexpected rejection type: ${error}`);
    }
    if (!error.message.includes(includes)) {
      throw new Error(
        `rejection ${JSON.stringify(error.message)} does not include ${
          JSON.stringify(includes)
        }`,
      );
    }
    return;
  }
  throw new Error("expected operation to reject");
}
