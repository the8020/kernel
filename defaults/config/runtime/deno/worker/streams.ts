export function trackStream(
  source: ReadableStream<Uint8Array>,
  onComplete: () => void,
  signal?: AbortSignal,
): ReadableStream<Uint8Array> {
  const reader = source.getReader();
  let complete = false;
  const finish = (): void => {
    if (complete) return;
    complete = true;
    signal?.removeEventListener("abort", abort);
    onComplete();
  };
  let abort: () => void;
  return new ReadableStream<Uint8Array>({
    start(controller) {
      abort = () => {
        controller.error(signal!.reason);
        finish();
        void reader.cancel(signal!.reason).catch(() => {});
      };
      if (signal?.aborted) abort();
      else signal?.addEventListener("abort", abort, { once: true });
    },
    async pull(controller) {
      try {
        const result = await reader.read();
        if (complete) return;
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
