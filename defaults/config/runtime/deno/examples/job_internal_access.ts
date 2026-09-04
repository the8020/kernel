export default async () => {
  let token = "visible";
  try {
    token = Deno.env.get("INTERNAL_API_TOKEN") ?? "missing";
  } catch (error) {
    token = error instanceof Error ? error.name : "denied";
  }
  let socket = "connected";
  try {
    const connection = await Deno.connect({
      transport: "unix",
      path: "/run/the8020/kernel.sock",
    });
    connection.close();
  } catch (error) {
    socket = error instanceof Error ? error.name : "denied";
  }
  return { token, socket };
};
