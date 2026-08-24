/**
 * OpenGTM OpenUI Component Library
 *
 * Domain-specific components for the AI chat — leads, stats, comparisons.
 * The LLM generates OpenUI Lang that maps to these React components.
 */

import type { ReactNode } from "react"
import {
  defineComponent as defineComponentBase,
  createLibrary,
  type DefinedComponent,
} from "@openuidev/react-lang"
import { z } from "zod"
import { useNavigate } from "react-router-dom"
import { BarChart, Bar, XAxis, YAxis, Tooltip, ResponsiveContainer, PieChart, Pie, Cell, CartesianGrid } from "recharts"

/**
 * Typed wrapper around `defineComponent`.
 *
 * The library's `ComponentRenderProps<P>` type does not surface the declared
 * prop fields (its underlying `@openuidev/lang-core` type erases the generic
 * payload), so the render function would otherwise receive an opaque wrapper
 * type instead of the schema's data shape. At runtime the evaluated props ARE
 * passed straight through as the inferred data shape, so we type the render
 * function as `(props: z.infer<T>) => ReactNode` — no runtime change, just an
 * accurate type for the props.
 */
function defineComponent<T extends z.ZodObject<z.ZodRawShape>>(config: {
  name: string
  description: string
  props: T
  component: (props: z.infer<T>) => ReactNode
}): DefinedComponent<T> {
  return defineComponentBase(
    config as unknown as Parameters<typeof defineComponentBase<T>>[0],
  )
}

/* ── Shared helpers ─────────────────────────────────────────────── */

const tierColor: Record<string, string> = {
  hot: "#ef4444",
  warm: "#f59e0b",
  cold: "#3b82f6",
  unqualified: "#6b7280",
}

function TierBadge({ tier }: { tier: string }) {
  const color = tierColor[tier] || "#6b7280"
  return (
    <span
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 4,
        padding: "2px 8px",
        borderRadius: 9999,
        fontSize: 11,
        fontWeight: 600,
        textTransform: "uppercase",
        letterSpacing: "0.05em",
        background: `${color}18`,
        color,
        border: `1px solid ${color}30`,
      }}
    >
      <span style={{ width: 6, height: 6, borderRadius: "50%", background: color }} />
      {tier}
    </span>
  )
}

/* ── ScoreBar ───────────────────────────────────────────────────── */

function ScoreBarView({ score, tier }: { score: number; tier: string }) {
  const color = tierColor[tier] || "#6b7280"
  return (
      <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
        <div
          style={{
            flex: 1,
            height: 6,
            borderRadius: 3,
            background: "hsl(var(--muted))",
            overflow: "hidden",
          }}
        >
          <div
            style={{
              width: `${Math.min(score, 100)}%`,
              height: "100%",
              borderRadius: 3,
              background: `linear-gradient(90deg, ${color}80, ${color})`,
              transition: "width 0.6s ease",
            }}
          />
        </div>
        <span style={{ fontSize: 13, fontWeight: 700, color, minWidth: 28 }}>{score}</span>
      </div>
    )
}

const ScoreBar = defineComponent({
  name: "ScoreBar",
  description: "Visual score indicator with tier coloring (0-100)",
  props: z.object({
    score: z.number().describe("Lead score 0-100"),
    tier: z.string().describe("Score tier: hot, warm, cold, unqualified"),
  }),
  component: ({ score, tier }) => <ScoreBarView score={score} tier={tier} />,
})

/* ── LeadCard ───────────────────────────────────────────────────── */

const LeadCard = defineComponent({
  name: "LeadCard",
  description: "Display a single lead with company info, score badge, and contact details. Use for individual lead display.",
  props: z.object({
    id: z.number().optional().describe("Lead ID for navigation"),
    company: z.string().describe("Company name"),
    city: z.string().optional().describe("City"),
    score: z.number().optional().describe("Lead score 0-100"),
    tier: z.string().optional().describe("hot, warm, cold, unqualified"),
    email: z.string().optional().describe("Contact email"),
    phone: z.string().optional().describe("Contact phone"),
    website: z.string().optional().describe("Company website"),
    specialization: z.string().optional().describe("Industry/specialization"),
    contact_person: z.string().optional().describe("Contact person name"),
    company_size: z.string().optional().describe("Employee count range"),
  }),
  component: (props) => {
    const nav = useNavigate()
    return (
      <div
        onClick={() => props.id && nav(`/leads/${props.id}`)}
        style={{
          padding: 16,
          borderRadius: 12,
          border: "1px solid hsl(var(--border) / 0.3)",
          background: "hsl(var(--card) / 0.6)",
          cursor: props.id ? "pointer" : "default",
          transition: "border-color 0.2s, background 0.2s",
          display: "flex",
          flexDirection: "column",
          gap: 10,
        }}
        onMouseEnter={(e) => {
          e.currentTarget.style.borderColor = "hsl(var(--primary) / 0.3)"
          e.currentTarget.style.background = "hsl(var(--card) / 0.8)"
        }}
        onMouseLeave={(e) => {
          e.currentTarget.style.borderColor = "hsl(var(--border) / 0.3)"
          e.currentTarget.style.background = "hsl(var(--card) / 0.6)"
        }}
      >
        {/* Header */}
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "flex-start" }}>
          <div>
            <div style={{ fontSize: 15, fontWeight: 600, color: "hsl(var(--foreground))" }}>{props.company}</div>
            {props.city && (
              <div style={{ fontSize: 12, color: "hsl(var(--muted-foreground))", marginTop: 2 }}>{props.city}</div>
            )}
          </div>
          {props.tier && <TierBadge tier={props.tier} />}
        </div>

        {/* Score */}
        {props.score != null && props.tier && (
          <ScoreBarView score={props.score} tier={props.tier} />
        )}

        {/* Details */}
        <div style={{ display: "flex", flexWrap: "wrap", gap: "6px 16px", fontSize: 12, color: "hsl(var(--muted-foreground))" }}>
          {props.specialization && <span>🏢 {props.specialization}</span>}
          {props.company_size && <span>👥 {props.company_size}</span>}
          {props.contact_person && <span>👤 {props.contact_person}</span>}
        </div>

        {/* Contact */}
        <div style={{ display: "flex", flexWrap: "wrap", gap: "4px 12px", fontSize: 12 }}>
          {props.email && (
            <a href={`mailto:${props.email}`} onClick={(e) => e.stopPropagation()}
              style={{ color: "hsl(var(--primary))", textDecoration: "none" }}>
              ✉ {props.email}
            </a>
          )}
          {props.phone && <span style={{ color: "hsl(var(--muted-foreground))" }}>📞 {props.phone}</span>}
          {props.website && (
            <a href={props.website.startsWith("http") ? props.website : `https://${props.website}`}
              target="_blank" rel="noopener noreferrer" onClick={(e) => e.stopPropagation()}
              style={{ color: "hsl(var(--primary) / 0.7)", textDecoration: "none" }}>
              🌐 {props.website}
            </a>
          )}
        </div>
      </div>
    )
  },
})

/* ── LeadTable ──────────────────────────────────────────────────── */

const LeadTable = defineComponent({
  name: "LeadTable",
  description: "Compact sortable table for displaying multiple leads. Use when showing 3+ leads.",
  props: z.object({
    leads: z.array(z.object({
      id: z.number().optional(),
      company: z.string(),
      city: z.string().optional(),
      score: z.number().optional(),
      tier: z.string().optional(),
      email: z.string().optional(),
      phone: z.string().optional(),
      specialization: z.string().optional(),
    })).describe("Array of lead objects"),
  }),
  component: ({ leads }) => {
    const nav = useNavigate()
    return (
      <div style={{ overflowX: "auto", borderRadius: 12, border: "1px solid hsl(var(--border) / 0.3)", background: "hsl(var(--card) / 0.5)" }}>
        <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 13 }}>
          <thead>
            <tr style={{ borderBottom: "1px solid hsl(var(--border) / 0.3)" }}>
              {["Company", "City", "Score", "Email", "Specialization"].map((h) => (
                <th key={h} style={{ padding: "10px 14px", textAlign: "left", fontSize: 11, fontWeight: 600, textTransform: "uppercase", letterSpacing: "0.05em", color: "hsl(var(--muted-foreground) / 0.7)" }}>{h}</th>
              ))}
            </tr>
          </thead>
          <tbody>
            {leads.map((l, i) => (
              <tr key={i}
                onClick={() => l.id && nav(`/leads/${l.id}`)}
                style={{ borderBottom: "1px solid hsl(var(--border) / 0.1)", cursor: l.id ? "pointer" : "default", transition: "background 0.15s" }}
                onMouseEnter={(e) => (e.currentTarget.style.background = "hsl(var(--muted) / 0.3)")}
                onMouseLeave={(e) => (e.currentTarget.style.background = "")}
              >
                <td style={{ padding: "10px 14px", fontWeight: 500, color: "hsl(var(--foreground))" }}>{l.company}</td>
                <td style={{ padding: "10px 14px", color: "hsl(var(--muted-foreground))" }}>{l.city || "—"}</td>
                <td style={{ padding: "10px 14px" }}>{l.tier ? <TierBadge tier={l.tier} /> : (l.score ?? "—")}</td>
                <td style={{ padding: "10px 14px", color: l.email ? "hsl(var(--primary))" : "hsl(var(--muted-foreground) / 0.4)" }}>{l.email || "—"}</td>
                <td style={{ padding: "10px 14px", color: "hsl(var(--muted-foreground))", maxWidth: 180, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{l.specialization || "—"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    )
  },
})

/* ── StatsPanel ─────────────────────────────────────────────────── */

const StatsPanel = defineComponent({
  name: "StatsPanel",
  description: "Pipeline statistics overview with metric cards. Use when showing pipeline stats or summaries.",
  props: z.object({
    total: z.number().describe("Total lead count"),
    hot: z.number().optional().describe("Hot tier count"),
    warm: z.number().optional().describe("Warm tier count"),
    cold: z.number().optional().describe("Cold tier count"),
    unqualified: z.number().optional().describe("Unqualified count"),
    with_email: z.number().optional().describe("Leads with email"),
    with_phone: z.number().optional().describe("Leads with phone"),
    avg_score: z.number().optional().describe("Average lead score"),
  }),
  component: (props) => {
    const metrics = [
      { label: "Total Leads", value: props.total, color: "hsl(var(--primary))" },
      { label: "Hot", value: props.hot, color: tierColor.hot },
      { label: "Warm", value: props.warm, color: tierColor.warm },
      { label: "Cold", value: props.cold, color: tierColor.cold },
    ].filter((m) => m.value != null)

    return (
      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(120px, 1fr))", gap: 10 }}>
        {metrics.map((m) => (
          <div key={m.label} style={{
            padding: "14px 16px",
            borderRadius: 10,
            border: `1px solid ${m.color}20`,
            background: `${m.color}08`,
            textAlign: "center",
          }}>
            <div style={{ fontSize: 22, fontWeight: 700, color: m.color }}>{m.value}</div>
            <div style={{ fontSize: 11, color: "hsl(var(--muted-foreground))", marginTop: 4, textTransform: "uppercase", letterSpacing: "0.05em" }}>{m.label}</div>
          </div>
        ))}
        {props.avg_score != null && (
          <div style={{ padding: "14px 16px", borderRadius: 10, border: "1px solid hsl(var(--border) / 0.3)", background: "hsl(var(--card) / 0.5)", textAlign: "center" }}>
            <div style={{ fontSize: 22, fontWeight: 700, color: "hsl(var(--foreground))" }}>{props.avg_score}</div>
            <div style={{ fontSize: 11, color: "hsl(var(--muted-foreground))", marginTop: 4, textTransform: "uppercase", letterSpacing: "0.05em" }}>Avg Score</div>
          </div>
        )}
        {props.with_email != null && (
          <div style={{ padding: "14px 16px", borderRadius: 10, border: "1px solid hsl(var(--border) / 0.3)", background: "hsl(var(--card) / 0.5)", textAlign: "center" }}>
            <div style={{ fontSize: 22, fontWeight: 700, color: "hsl(var(--foreground))" }}>{props.with_email}</div>
            <div style={{ fontSize: 11, color: "hsl(var(--muted-foreground))", marginTop: 4, textTransform: "uppercase", letterSpacing: "0.05em" }}>With Email</div>
          </div>
        )}
      </div>
    )
  },
})

/* ── CompareGrid ────────────────────────────────────────────────── */

const CompareGrid = defineComponent({
  name: "CompareGrid",
  description: "Side-by-side comparison of 2-3 leads. Use when comparing leads.",
  props: z.object({
    leads: z.array(z.object({
      company: z.string(),
      city: z.string().optional(),
      score: z.number().optional(),
      tier: z.string().optional(),
      specialization: z.string().optional(),
      company_size: z.string().optional(),
      has_email: z.boolean().optional(),
      has_phone: z.boolean().optional(),
      has_linkedin: z.boolean().optional(),
      data_completeness: z.number().optional(),
    })).describe("2-3 leads to compare"),
  }),
  component: ({ leads }) => {
    const rows = ["Score", "Tier", "City", "Specialization", "Size", "Email", "Phone", "LinkedIn", "Completeness"]
    const getValue = (l: typeof leads[0], row: string) => {
      switch (row) {
        case "Score": return l.score ?? "—"
        case "Tier": return l.tier ? <TierBadge tier={l.tier} /> : "—"
        case "City": return l.city || "—"
        case "Specialization": return l.specialization || "—"
        case "Size": return l.company_size || "—"
        case "Email": return l.has_email ? "✅" : "❌"
        case "Phone": return l.has_phone ? "✅" : "❌"
        case "LinkedIn": return l.has_linkedin ? "✅" : "❌"
        case "Completeness": return l.data_completeness != null ? `${l.data_completeness}%` : "—"
        default: return "—"
      }
    }

    return (
      <div style={{ overflowX: "auto", borderRadius: 12, border: "1px solid hsl(var(--border) / 0.3)", background: "hsl(var(--card) / 0.5)" }}>
        <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 13 }}>
          <thead>
            <tr style={{ borderBottom: "1px solid hsl(var(--border) / 0.3)" }}>
              <th style={{ padding: "10px 14px", textAlign: "left", fontSize: 11, fontWeight: 600, color: "hsl(var(--muted-foreground) / 0.6)" }}></th>
              {leads.map((l, i) => (
                <th key={i} style={{ padding: "10px 14px", textAlign: "center", fontWeight: 600, color: "hsl(var(--foreground))" }}>{l.company}</th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.map((row) => (
              <tr key={row} style={{ borderBottom: "1px solid hsl(var(--border) / 0.1)" }}>
                <td style={{ padding: "8px 14px", fontSize: 12, color: "hsl(var(--muted-foreground))", fontWeight: 500 }}>{row}</td>
                {leads.map((l, i) => (
                  <td key={i} style={{ padding: "8px 14px", textAlign: "center" }}>{getValue(l, row)}</td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    )
  },
})

/* ── InfoCallout ────────────────────────────────────────────────── */

const InfoCallout = defineComponent({
  name: "InfoCallout",
  description: "Tip, warning, or suggestion callout. Use for recommendations and insights.",
  props: z.object({
    type: z.enum(["tip", "warning", "info", "success"]).describe("Callout type"),
    title: z.string().describe("Callout title"),
    content: z.string().describe("Callout body text"),
  }),
  component: ({ type, title, content }) => {
    const styles: Record<string, { bg: string; border: string; icon: string }> = {
      tip: { bg: "hsl(var(--primary) / 0.05)", border: "hsl(var(--primary) / 0.2)", icon: "💡" },
      warning: { bg: "#f59e0b08", border: "#f59e0b30", icon: "⚠️" },
      info: { bg: "#3b82f608", border: "#3b82f630", icon: "ℹ️" },
      success: { bg: "#22c55e08", border: "#22c55e30", icon: "✅" },
    }
    const s = styles[type] || styles.info
    return (
      <div style={{ padding: "12px 16px", borderRadius: 10, border: `1px solid ${s.border}`, background: s.bg, display: "flex", gap: 10, alignItems: "flex-start" }}>
        <span style={{ fontSize: 16, lineHeight: 1 }}>{s.icon}</span>
        <div>
          <div style={{ fontSize: 13, fontWeight: 600, color: "hsl(var(--foreground))", marginBottom: 4 }}>{title}</div>
          <div style={{ fontSize: 12, color: "hsl(var(--muted-foreground))", lineHeight: 1.5 }}>{content}</div>
        </div>
      </div>
    )
  },
})

/* ── OutreachDraft ──────────────────────────────────────────────── */

const OutreachDraft = defineComponent({
  name: "OutreachDraft",
  description: "Email or LinkedIn outreach message preview with copy button.",
  props: z.object({
    channel: z.enum(["email", "linkedin"]).describe("Outreach channel"),
    subject: z.string().optional().describe("Email subject line"),
    body: z.string().describe("Message body"),
    to: z.string().optional().describe("Recipient email or profile URL"),
  }),
  component: ({ channel, subject, body, to }) => {
    return (
      <div style={{ borderRadius: 12, border: "1px solid hsl(var(--border) / 0.3)", background: "hsl(var(--card) / 0.6)", overflow: "hidden" }}>
        {/* Header */}
        <div style={{ padding: "10px 16px", borderBottom: "1px solid hsl(var(--border) / 0.2)", display: "flex", justifyContent: "space-between", alignItems: "center", background: "hsl(var(--muted) / 0.3)" }}>
          <span style={{ fontSize: 12, fontWeight: 600, color: "hsl(var(--muted-foreground))", textTransform: "uppercase", letterSpacing: "0.05em" }}>
            {channel === "email" ? "📧 Email Draft" : "💼 LinkedIn Message"}
          </span>
          {to && <span style={{ fontSize: 11, color: "hsl(var(--muted-foreground) / 0.7)" }}>To: {to}</span>}
        </div>
        {/* Content */}
        <div style={{ padding: 16 }}>
          {subject && <div style={{ fontSize: 14, fontWeight: 600, color: "hsl(var(--foreground))", marginBottom: 10 }}>Subject: {subject}</div>}
          <div style={{ fontSize: 13, color: "hsl(var(--foreground) / 0.85)", lineHeight: 1.7, whiteSpace: "pre-wrap" }}>{body}</div>
        </div>
      </div>
    )
  },
})

/* ── Charts ─────────────────────────────────────────────────────── */

const SimpleBarChart = defineComponent({
  name: "SimpleBarChart",
  description: "A bar chart. Use when the user asks for a chart or visual distribution (e.g. leads by tier).",
  props: z.object({
    title: z.string().describe("Chart title"),
    labels: z.array(z.string()).describe("X-axis labels"),
    values: z.array(z.number()).describe("Y-axis values corresponding to the labels"),
    color: z.string().optional().describe("Hex color code for bars"),
  }),
  component: ({ title, labels, values, color }) => {
    const data = labels.map((name, i) => ({ name, value: values[i] || 0 }));
    return (
      <div style={{ padding: 16, borderRadius: 12, border: "1px solid hsl(var(--border) / 0.3)", background: "hsl(var(--card) / 0.5)" }}>
        <div style={{ fontSize: 14, fontWeight: 600, color: "hsl(var(--foreground))", marginBottom: 16, textAlign: "center" }}>{title}</div>
        <div style={{ height: 250, width: "100%" }}>
          <ResponsiveContainer width="100%" height="100%">
            <BarChart data={data}>
              <CartesianGrid strokeDasharray="3 3" stroke="hsl(var(--border) / 0.3)" vertical={false} />
              <XAxis dataKey="name" stroke="hsl(var(--muted-foreground))" fontSize={11} tickLine={false} axisLine={false} />
              <YAxis stroke="hsl(var(--muted-foreground))" fontSize={11} tickLine={false} axisLine={false} />
              <Tooltip
                contentStyle={{ background: "hsl(var(--card))", border: "1px solid hsl(var(--border))", borderRadius: 8, fontSize: 12 }}
                itemStyle={{ color: "hsl(var(--foreground))" }}
                cursor={{ fill: "hsl(var(--muted) / 0.5)" }}
              />
              <Bar dataKey="value" fill={color || "hsl(var(--primary))"} radius={[4, 4, 0, 0]} />
            </BarChart>
          </ResponsiveContainer>
        </div>
      </div>
    )
  },
})

const SimplePieChart = defineComponent({
  name: "SimplePieChart",
  description: "A pie chart. Use for distributions like leads by city or source.",
  props: z.object({
    title: z.string().describe("Chart title"),
    labels: z.array(z.string()).describe("Labels for the slices"),
    values: z.array(z.number()).describe("Values corresponding to the labels"),
  }),
  component: ({ title, labels, values }) => {
    const COLORS = ["#3b82f6", "#10b981", "#f59e0b", "#ef4444", "#8b5cf6", "#ec4899", "#6366f1"];
    const data = labels.map((name, i) => ({ name, value: values[i] || 0 }));
    return (
      <div style={{ padding: 16, borderRadius: 12, border: "1px solid hsl(var(--border) / 0.3)", background: "hsl(var(--card) / 0.5)" }}>
        <div style={{ fontSize: 14, fontWeight: 600, color: "hsl(var(--foreground))", marginBottom: 8, textAlign: "center" }}>{title}</div>
        <div style={{ height: 250, width: "100%" }}>
          <ResponsiveContainer width="100%" height="100%">
            <PieChart>
              <Tooltip
                contentStyle={{ background: "hsl(var(--card))", border: "1px solid hsl(var(--border))", borderRadius: 8, fontSize: 12 }}
                itemStyle={{ color: "hsl(var(--foreground))" }}
              />
              <Pie data={data} innerRadius={60} outerRadius={90} paddingAngle={2} dataKey="value" nameKey="name" stroke="none">
                {data.map((_, index) => (
                  <Cell key={`cell-${index}`} fill={COLORS[index % COLORS.length]} />
                ))}
              </Pie>
            </PieChart>
          </ResponsiveContainer>
        </div>
        <div style={{ display: "flex", flexWrap: "wrap", justifyContent: "center", gap: 12, marginTop: 8 }}>
          {data.map((entry, index) => (
            <div key={`legend-${index}`} style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 11, color: "hsl(var(--muted-foreground))" }}>
              <span style={{ width: 8, height: 8, borderRadius: "50%", background: COLORS[index % COLORS.length] }} />
              {entry.name}
            </div>
          ))}
        </div>
      </div>
    )
  },
})

/* ── Root ───────────────────────────────────────────────────────── */

const Root = defineComponent({
  name: "Root",
  description: "The root container for the UI response. Must be used as the root element.",
  props: z.object({
    children: z.array(z.any()).describe("List of components to render"),
  }),
  component: ({ children }) => {
    return <div style={{ display: "flex", flexDirection: "column", gap: 16, margin: "12px 0" }}>{children}</div>
  },
})

/* ── Library ────────────────────────────────────────────────────── */

export const yupchLibrary = createLibrary({
  components: [Root, ScoreBar, LeadCard, LeadTable, StatsPanel, CompareGrid, InfoCallout, OutreachDraft, SimpleBarChart, SimplePieChart],
  root: "Root",
})

/**
 * Generate the OpenUI Lang system prompt section.
 * Call this on the backend to append to the LLM system prompt.
 */
export function generatePromptSpec(): string {
  return yupchLibrary.prompt({
    preamble: "",
    additionalRules: [
      "Use LeadCard for showing a single lead in detail.",
      "Use LeadTable when displaying 3+ leads in a list.",
      "Use StatsPanel for pipeline statistics and metrics.",
      "Use SimpleBarChart or SimplePieChart when asked to draw a chart or visualize distributions.",
      "CRITICAL: For SimpleBarChart and SimplePieChart, pass the title, labels array, and values array positionally exactly as shown in the examples.",
      "Use CompareGrid when comparing 2-3 leads side by side.",
      "Use InfoCallout for tips, warnings, and actionable insights.",
      "Use OutreachDraft for email/LinkedIn message previews.",
      "For conversational text, explanations, or simple answers, respond in plain markdown — do NOT use OpenUI Lang.",
      "Only use OpenUI Lang when displaying structured data (leads, stats, comparisons).",
    ],
    examples: [
      "root = Root([pie_chart, bar_chart])",
      "pie_chart = SimplePieChart(\"Lead Tiers\", [\"Hot\", \"Warm\", \"Cold\"], [21, 105, 152])",
      "bar_chart = SimpleBarChart(\"Leads by City\", [\"Bangalore\", \"Mumbai\"], [161, 44], \"#4CAF50\")",
      "leads_table = LeadTable([{ company: \"Stark Industries\", city: \"New York\" }])",
      "stats = StatsPanel(500, 100, 200, 150, 50)"
    ]
  })
}
