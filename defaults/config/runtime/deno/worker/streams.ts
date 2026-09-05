export function trackStream(
  source: ReadableStream<Uint8Array>,
  onComplete: () => void,
): ReadableStream<Uint8Array> {
  const reader = source.getReader();
  let complete = false;
  const finish = (): void => {
    if (complete) return;
    complete = true;
    onComplete();
  };
  return new ReadableStream<Uint8Array>({
    async pull(controller) {
      try {
        const result = await reader.read();
        if (result.done) {
          finish();
          controller.close();
        } else {
          controller.enqueue(result.value);
        }
      } catch (error) {
        finish();
        controller.error(error);
      }
    },
    async cancel(reason) {
      try {
        await reader.cancel(reason);
      } finally {
        finish();
      }
    },
  });
}
