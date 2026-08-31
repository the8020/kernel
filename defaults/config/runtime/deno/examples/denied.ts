export async function run(): Promise<string> {
  return await Deno.readTextFile("/etc/passwd");
}
