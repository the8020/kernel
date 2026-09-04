export default async function fetchExternal(url: string): Promise<string> {
  return await (await globalThis.fetch(url)).text();
}
