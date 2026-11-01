export type Call = { id: string; name: string; args: string };
export type Block = {
  kind: number;
  text?: string;
  thinking?: string;
  tool_call?: Call;
  tool_result?: {
    tool_call_id: string;
    name: string;
    content: string;
    is_error: boolean;
  };
};
export type Message = {
  role: "user" | "assistant" | "tool" | "system";
  blocks: Block[];
};
export type State = {
  session_id: string;
  run_id: string;
  status: string;
  messages: Message[];
  streaming: string;
  thinking: string;
  activity: string;
  error: string;
  usage: { PromptTokens: number; CompletionTokens: number };
  approvals: { id: string; call: Call }[];
  workers: { name: string; status: string; tool: string }[];
};
export type Session = { id: string; title: string };
export type Config = { cwd: string; model: string; mode: string };
// UI components depend on this transport, not a desktop-specific global.
export async function request<T>(path: string, body?: unknown): Promise<T> {
  const response = await fetch(`/api/${path}`, {
    method: body === undefined ? "GET" : "POST",
    credentials: "same-origin",
    headers: body === undefined ? {} : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await response.json();
  if (!response.ok)
    throw new Error(data.error || `请求失败 (${response.status})`);
  return data as T;
}
export const emptyState: State = {
  session_id: "",
  run_id: "",
  status: "idle",
  messages: [],
  streaming: "",
  thinking: "",
  activity: "",
  error: "",
  usage: { PromptTokens: 0, CompletionTokens: 0 },
  approvals: [],
  workers: [],
};
