export default async function nested(): Promise<string> {
  const source = `self.onmessage = () => self.postMessage("nested-ok")`;
  const worker = new Worker(
    `data:application/javascript,${encodeURIComponent(source)}`,
    { type: "module", name: "nested-test" },
  );
  const result = await new Promise<string>((resolve) => {
    worker.onmessage = (event) => resolve(event.data as string);
    worker.postMessage(null);
  });
  worker.terminate();
  return result;
}
