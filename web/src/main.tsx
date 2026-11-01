import React, { useEffect, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import {
  ArrowUp,
  Check,
  ChevronDown,
  ChevronRight,
  Code2,
  Copy,
  FileCode2,
  Folder,
  GitBranch,
  LoaderCircle,
  MessageSquare,
  PanelLeftClose,
  PanelLeftOpen,
  Plus,
  Search,
  Settings2,
  ShieldCheck,
  Sparkles,
  Square,
  Terminal,
  X,
} from "lucide-react";
import { Config, emptyState, Message, request, Session, State } from "./api";
import "./styles.css";

function Markdown({ text }: { text: string }) {
  return (
    <ReactMarkdown
      remarkPlugins={[remarkGfm]}
      components={{
        a: ({ children, href }) => (
          <a href={href} target="_blank" rel="noreferrer">
            {children}
          </a>
        ),
      }}
    >
      {text}
    </ReactMarkdown>
  );
}
function MessageView({
  message,
  results,
  active,
}: {
  message: Message;
  results: Map<string, { content: string; is_error: boolean }>;
  active: boolean;
}) {
  const [copied, setCopied] = useState(false);
  if (message.role === "tool" || message.role === "system") return null;
  const text = message.blocks
    .filter((b) => b.kind === 0)
    .map((b) => b.text || "")
    .join("\n");
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      setTimeout(() => setCopied(false), 1600);
    } catch {
      setCopied(false);
    }
  };
  return (
    <article className={`message ${message.role}`}>
      {message.role === "assistant" && (
        <div className="assistant-label">
          <span className="avatar small">
            <Code2 size={16} />
          </span>
          CodeClaw
        </div>
      )}
      <div className="message-body">
        {message.blocks.map((b, i) => {
          if (b.kind === 0)
            return (
              <div className="prose" key={i}>
                <Markdown text={b.text || ""} />
              </div>
            );
          if (b.kind === 1 && b.thinking)
            return (
              <details className="thinking" key={i}>
                <summary>思考过程</summary>
                <div className="prose">
                  <Markdown text={b.thinking} />
                </div>
              </details>
            );
          if (b.tool_call) {
            const c = b.tool_call,
              result = results.get(c.id);
            return (
              <details
                className={`tool-card ${result?.is_error ? "tool-error" : ""}`}
                key={i}
              >
                <summary>
                  {result ? (
                    result.is_error ? (
                      <X size={15} />
                    ) : (
                      <Check size={15} />
                    )
                  ) : active ? (
                    <LoaderCircle className="spin" size={15} />
                  ) : (
                    <Terminal size={15} />
                  )}
                  <span>{c.name}</span>
                  <span className="tool-status">
                    {result
                      ? result.is_error
                        ? "执行失败"
                        : "执行完成"
                      : active
                        ? "执行中"
                        : "未完成"}
                  </span>
                  <ChevronDown size={14} />
                </summary>
                <div className="tool-detail">
                  <label>调用参数</label>
                  <pre>{pretty(c.args)}</pre>
                  {result && (
                    <>
                      <label>执行结果</label>
                      <pre>{result.content}</pre>
                    </>
                  )}
                </div>
              </details>
            );
          }
          return null;
        })}
      </div>
      {message.role === "assistant" && text && (
        <button
          className="icon-button copy"
          aria-label="复制回复"
          onClick={copy}
        >
          {copied ? <Check size={15} /> : <Copy size={15} />}
        </button>
      )}
    </article>
  );
}
function pretty(text: string) {
  try {
    return JSON.stringify(JSON.parse(text), null, 2);
  } catch {
    return text;
  }
}
const suggestions = [
  {
    icon: <Code2 size={18} />,
    title: "了解这个项目",
    detail: "梳理目录结构与核心实现",
    prompt:
      "请阅读当前项目，介绍目录结构、核心模块和运行方式。先只分析，不修改文件。",
  },
  {
    icon: <GitBranch size={18} />,
    title: "检查代码变更",
    detail: "审查改动，发现潜在问题",
    prompt:
      "请审查当前工作区的代码变更，指出潜在缺陷并标注文件位置。先不要修改代码。",
  },
  {
    icon: <Terminal size={18} />,
    title: "运行项目测试",
    detail: "定位失败原因并给出建议",
    prompt:
      "请查看项目的测试说明，运行合适的测试并总结结果。需要执行命令时按现有权限规则处理。",
  },
];
function App() {
  const [state, setState] = useState<State>(emptyState),
    [config, setConfig] = useState<Config | null>(null),
    [sessions, setSessions] = useState<Session[]>([]);
  const [connected, setConnected] = useState(false),
    [error, setError] = useState(""),
    [text, setText] = useState(""),
    [search, setSearch] = useState(""),
    [showSearch, setShowSearch] = useState(false);
  const [sidebar, setSidebar] = useState(window.innerWidth > 760),
    [settings, setSettings] = useState(false),
    [pending, setPending] = useState(false),
    [notice, setNotice] = useState("");
  const scroll = useRef<HTMLDivElement>(null),
    input = useRef<HTMLTextAreaElement>(null),
    follow = useRef(true);
  const active = state.status === "running" || state.status === "stopping";
  const refreshSessions = () =>
    request<Session[]>("sessions")
      .then(setSessions)
      .catch((e) => setError(e.message));
  useEffect(() => {
    let source: EventSource | undefined,
      disposed = false,
      retry: ReturnType<typeof setTimeout>;
    async function connect() {
      try {
        const c = await request<Config>("bootstrap");
        if (disposed) return;
        setConfig(c);
        source = new EventSource("/api/events");
        source.addEventListener("state", (event) => {
          if (!disposed) {
            setState(JSON.parse((event as MessageEvent).data));
            setConnected(true);
            setError("");
          }
        });
        source.onopen = () => {
          if (!disposed) setConnected(true);
        };
        source.onerror = () => {
          source?.close();
          if (!disposed) {
            setConnected(false);
            retry = setTimeout(connect, 2000);
          }
        };
        refreshSessions();
      } catch {
        if (!disposed) {
          setConnected(false);
          retry = setTimeout(connect, 2000);
        }
      }
    }
    connect();
    return () => {
      disposed = true;
      clearTimeout(retry);
      source?.close();
    };
  }, []);
  useEffect(() => {
    if (connected) refreshSessions();
  }, [state.session_id, state.status]);
  useEffect(() => {
    if (follow.current)
      scroll.current?.scrollTo({
        top: scroll.current.scrollHeight,
        behavior: "instant",
      });
  }, [state]);
  useEffect(() => {
    if (input.current) {
      input.current.style.height = "auto";
      input.current.style.height =
        Math.min(input.current.scrollHeight, 180) + "px";
    }
  }, [text]);
  async function act(fn: () => Promise<unknown>) {
    setError("");
    setPending(true);
    try {
      await fn();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setPending(false);
    }
  }
  async function switchSession(id = "") {
    await act(async () => {
      await request("sessions", { id });
      setState(await request<State>("state"));
      setText("");
      follow.current = true;
      if (window.innerWidth <= 760) setSidebar(false);
      await refreshSessions();
    });
  }
  async function send() {
    if (!text.trim() || !connected || pending || state.status === "stopping")
      return;
    const value = text;
    await act(async () => {
      await request(active ? "steer" : "runs", { text: value });
      setText("");
      follow.current = true;
      if (active) {
        setNotice("补充指令已排队，将在下一步模型请求前送达");
        setTimeout(() => setNotice(""), 4500);
      }
    });
  }
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setSettings(false);
        if (window.innerWidth <= 760) setSidebar(false);
      }
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        if (!active && !pending && connected) switchSession();
      }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [active, pending, connected]);
  const results = new Map<string, { content: string; is_error: boolean }>();
  for (const m of state.messages || [])
    for (const b of m.blocks)
      if (b.tool_result) results.set(b.tool_result.tool_call_id, b.tool_result);
  const isEmpty = !(state.messages || []).some(
    (m) => m.role === "user" || m.role === "assistant",
  );
  const project = config?.cwd.split("/").filter(Boolean).pop() || "当前项目";
  const statusLabel = !connected
    ? "连接中"
    : state.approvals.length
      ? "等待审批"
      : active
        ? state.status === "stopping"
          ? "正在停止"
          : "运行中"
        : "本地已连接";
  return (
    <div className={`app ${sidebar ? "" : "sidebar-hidden"}`}>
      {sidebar && (
        <div className="mobile-overlay" onClick={() => setSidebar(false)} />
      )}
      <aside className="sidebar">
        <div className="sidebar-brand">
          <div className="brand">
            <span className="avatar">
              <Code2 size={21} />
            </span>
            <strong>CodeClaw</strong>
          </div>
          <button
            className="icon-button"
            aria-label="收起侧栏"
            onClick={() => setSidebar(false)}
          >
            <PanelLeftClose size={19} />
          </button>
        </div>
        <nav>
          <button
            className="nav-button"
            onClick={() => switchSession()}
            disabled={active || pending || !connected}
          >
            <Plus size={18} />
            <span>新对话</span>
            <kbd>⌘ K</kbd>
          </button>
          <button
            className="nav-button"
            onClick={() => setShowSearch(!showSearch)}
          >
            <Search size={18} />
            <span>搜索对话</span>
          </button>
        </nav>
        {showSearch && (
          <input
            autoFocus
            className="search"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="搜索历史对话…"
            aria-label="搜索历史对话"
          />
        )}
        <div className="project-card">
          <Folder size={17} />
          <div>
            <strong title={config?.cwd}>{project}</strong>
            <small>本地工作区</small>
          </div>
          <span className="status-dot" />
        </div>
        <div className="history-heading">
          最近对话<span>{sessions.length}</span>
        </div>
        <div className="history">
          {sessions
            .filter(
              (s) =>
                !showSearch ||
                s.title.toLowerCase().includes(search.toLowerCase()),
            )
            .map((s) => (
              <button
                disabled={active || pending || !connected}
                className={`session ${s.id === state.session_id ? "selected" : ""}`}
                key={s.id}
                title={s.title}
                onClick={() => switchSession(s.id)}
              >
                <MessageSquare size={15} />
                <span>{s.title}</span>
              </button>
            ))}
          {sessions.length === 0 && (
            <p className="history-empty">你的对话会保存在这里</p>
          )}
        </div>
        <button className="sidebar-footer" onClick={() => setSettings(true)}>
          <span className="user-avatar">L</span>
          <span>
            <strong>本地工作空间</strong>
            <small>本地执行，按需审批</small>
          </span>
          <Settings2 size={17} />
        </button>
      </aside>
      <main>
        <header>
          <div className="header-left">
            {!sidebar && (
              <button
                className="icon-button"
                aria-label="展开侧栏"
                onClick={() => setSidebar(true)}
              >
                <PanelLeftOpen size={20} />
              </button>
            )}
            <button className="model-button" onClick={() => setSettings(true)}>
              CodeClaw <ChevronDown size={16} />
            </button>
            <span className="model-tag">{config?.model || "Agent"}</span>
          </div>
          <button
            className={`connection ${connected ? "online" : ""}`}
            onClick={() => setSettings(true)}
          >
            <span className="status-dot" />
            {statusLabel}
          </button>
        </header>
        <div
          className="conversation"
          ref={scroll}
          onScroll={() => {
            if (scroll.current)
              follow.current =
                scroll.current.scrollHeight -
                  scroll.current.scrollTop -
                  scroll.current.clientHeight <
                100;
          }}
        >
          {isEmpty ? (
            <section className="welcome">
              <span className="welcome-icon">
                <Code2 size={32} strokeWidth={1.5} />
              </span>
              <div className="eyebrow">你的本地编程搭档</div>
              <h1>今天，我们写点什么？</h1>
              <p>从一个想法开始，让 CodeClaw 和你一起完成。</p>
              <div className="suggestions">
                {suggestions.map((s) => (
                  <button
                    key={s.title}
                    onClick={() => {
                      setText(s.prompt);
                      input.current?.focus();
                    }}
                  >
                    <span className="suggestion-icon">{s.icon}</span>
                    <strong>{s.title}</strong>
                    <small>{s.detail}</small>
                    <ChevronRight className="suggestion-arrow" size={15} />
                  </button>
                ))}
              </div>
              <div className="workspace-hint">
                <Folder size={14} />
                <span>{project}</span>
                <span className="dot-separator">·</span>
                <span>基于当前工作区协作</span>
              </div>
            </section>
          ) : (
            <div className="messages">
              {state.messages.map((m, i) => (
                <MessageView
                  key={`${state.session_id}-${i}`}
                  message={m}
                  results={results}
                  active={active}
                />
              ))}
              {state.thinking && (
                <details className="thinking">
                  <summary>正在思考…</summary>
                  <div className="prose">
                    <Markdown text={state.thinking} />
                  </div>
                </details>
              )}
              {state.streaming && (
                <article className="message assistant">
                  <div className="assistant-label">
                    <span className="avatar small">
                      <Code2 size={16} />
                    </span>
                    CodeClaw
                  </div>
                  <div className="prose">
                    <Markdown text={state.streaming} />
                  </div>
                </article>
              )}
              {active && (
                <div className="activity">
                  <LoaderCircle className="spin" size={15} />
                  {state.activity || "正在运行"}
                </div>
              )}
              {state.status === "cancelled" && (
                <div className="run-note">已停止。已完成的文件修改会保留。</div>
              )}
              {state.status === "completed" && (
                <div className="run-note">
                  <Check size={13} />
                  本轮已结束
                  {state.usage.CompletionTokens > 0 &&
                    ` · ${state.usage.CompletionTokens.toLocaleString()} 输出 tokens`}
                </div>
              )}
            </div>
          )}
        </div>
        <div className="composer-region">
          {state.workers.length > 0 && (
            <details className="workers">
              <summary>
                <GitBranch size={14} />子 Agent · {state.workers.length}
                <ChevronDown size={14} />
              </summary>
              <div>
                {state.workers.map((w) => (
                  <div className="worker" key={w.name}>
                    <span>{w.name}</span>
                    <span>{w.tool || w.status}</span>
                  </div>
                ))}
              </div>
            </details>
          )}
          {(error || state.error) && (
            <div className="error-banner" role="alert">
              {error || state.error}
            </div>
          )}
          {notice && (
            <div className="notice" role="status">
              {notice}
            </div>
          )}
          {!connected && (
            <div className="notice">
              正在连接本地 Agent 服务，连接恢复后可继续对话。
            </div>
          )}
          {state.approvals.map((a) => (
            <div className="approval" key={a.id}>
              <div className="approval-title">
                <ShieldCheck size={18} />
                <strong>需要你的批准</strong>
                <span>工具执行</span>
              </div>
              <div className="approval-name">{a.call.name}</div>
              <pre>{pretty(a.call.args)}</pre>
              <div className="approval-actions">
                <button
                  disabled={pending}
                  onClick={() =>
                    act(() => request(`approvals/${a.id}`, { allow: false }))
                  }
                >
                  拒绝
                </button>
                <button
                  className="primary"
                  disabled={pending}
                  onClick={() =>
                    act(() => request(`approvals/${a.id}`, { allow: true }))
                  }
                >
                  允许本次执行
                </button>
              </div>
            </div>
          ))}
          <div className="composer">
            <textarea
              ref={input}
              aria-label="发送任务给 CodeClaw"
              placeholder={
                active
                  ? "补充指令，调整接下来的方向…"
                  : "描述任务，或提出一个问题…"
              }
              rows={1}
              value={text}
              onChange={(e) => setText(e.target.value)}
              onKeyDown={(e) => {
                if (
                  e.key === "Enter" &&
                  !e.shiftKey &&
                  !e.nativeEvent.isComposing
                ) {
                  e.preventDefault();
                  send();
                }
              }}
            />
            <div className="composer-toolbar">
              <button
                className="workspace-button"
                title={config?.cwd}
                onClick={() => setSettings(true)}
              >
                <Folder size={16} />
                <span>{project}</span>
              </button>
              <div className="composer-right">
                <span className="permission-label">
                  <ShieldCheck size={13} />
                  {config?.mode === "always-ask"
                    ? "逐次审批"
                    : config?.mode === "yolo"
                      ? "自动执行"
                      : "受控执行"}
                </span>
                {active && (
                  <button
                    className="stop-button"
                    aria-label="停止运行及子任务"
                    title="停止运行及子任务"
                    disabled={pending || state.status === "stopping"}
                    onClick={() => act(() => request("cancel", {}))}
                  >
                    <Square size={14} fill="currentColor" />
                  </button>
                )}
                <button
                  className="send-button"
                  aria-label={active ? "发送补充指令" : "发送任务"}
                  title={active ? "发送补充指令" : "发送任务"}
                  disabled={
                    !connected ||
                    !text.trim() ||
                    pending ||
                    state.status === "stopping"
                  }
                  onClick={send}
                >
                  <ArrowUp size={21} />
                </button>
              </div>
            </div>
          </div>
          <div className="composer-footnote">
            CodeClaw 会在本地执行操作，请留意工具调用与审批。
            <span>Shift + Enter 换行</span>
          </div>
        </div>
      </main>
      {settings && (
        <div className="modal-backdrop" onClick={() => setSettings(false)}>
          <section
            className="settings-modal"
            role="dialog"
            aria-modal="true"
            aria-label="工作区信息"
            onClick={(e) => e.stopPropagation()}
          >
            <div className="modal-title">
              <h2>工作区信息</h2>
              <button
                autoFocus
                className="icon-button"
                aria-label="关闭工作区信息"
                onClick={() => setSettings(false)}
              >
                <X size={20} />
              </button>
            </div>
            <p>当前使用本地 Agent 配置，模型密钥不会发送到浏览器。</p>
            <dl>
              <dt>
                <Folder size={16} />
                工作目录
              </dt>
              <dd>{config?.cwd || "连接中"}</dd>
              <dt>
                <Sparkles size={16} />
                模型
              </dt>
              <dd>{config?.model || "—"}</dd>
              <dt>
                <ShieldCheck size={16} />
                审批策略
              </dt>
              <dd>{config?.mode || "—"}</dd>
              <dt>
                <FileCode2 size={16} />
                会话
              </dt>
              <dd>{state.session_id || "—"}</dd>
            </dl>
            <div className="settings-note">
              切换项目需用目标目录重新启动服务：
              <code>--cwd /path/to/project</code>
            </div>
          </section>
        </div>
      )}
    </div>
  );
}
createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
