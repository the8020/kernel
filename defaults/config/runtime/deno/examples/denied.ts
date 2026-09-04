export default async function denied(): Promise<string> {
  return await Deno.readTextFile("/etc/passwd");
}
