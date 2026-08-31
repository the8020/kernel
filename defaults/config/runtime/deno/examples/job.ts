export function run(
  input: unknown,
  context: {
    executionCount: number;
    log(event: { level: "info"; message: string }): void;
  },
): unknown {
  context.log({
    level: "info",
    message: `execution ${context.executionCount}`,
  });
  console.log("job input", input);
  return { input, executionCount: context.executionCount };
}
