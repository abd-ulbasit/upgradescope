// Wire types mirroring the Go server's JSON (internal/server, registry).
// Field names match the Go struct tags exactly — keep in sync by hand.

export type Severity = "blocker" | "warning" | "info";

export interface EvalSummary {
  target: string;
  score: number;
  ready: boolean;
  blockers: number;
  warnings: number;
  kbVersion: string;
  evaluatedAt: string;
}

export interface Cluster {
  id: number;
  name: string;
  clusterUid: string;
  firstSeen: string;
  lastSeen: string;
}

export interface CapabilityStatus {
  available: boolean;
  reason?: string;
}

export interface ClusterDetail extends Cluster {
  capabilities?: Record<string, CapabilityStatus>;
  evaluations: EvalSummary[];
}

export interface Finding {
  category: string;
  severity: Severity;
  key?: string;
  title: string;
  detail: string;
  teams?: string[];
  namespaces?: string[];
  remediation?: string;
  citations?: string[];
}

export interface CapabilityGap {
  capability: string;
  reason: string;
}

export interface TeamScore {
  score: number;
  ready: boolean;
  blockers: number;
  warnings: number;
}

export interface Report {
  clusterId: string;
  target: string;
  kbVersion: string;
  score: number;
  ready: boolean;
  findings: Finding[];
  notAssessed?: CapabilityGap[];
  teams?: Record<string, TeamScore>;
}

export interface FindingsResponse {
  target: string;
  findings: Finding[];
}

export interface ScorePoint {
  at: string;
  score: number;
  ready: boolean;
}

export interface FleetCell {
  score: number;
  ready: boolean;
  blockers: number;
}

export interface FleetRow {
  clusterId: number;
  name: string;
  cells: Record<string, FleetCell | null>;
}

export interface FleetResponse {
  targets: string[];
  clusters: FleetRow[];
}

export interface AddOnCompat {
  range: string;
  k8s_min: string;
  k8s_max: string;
  citations: string[];
}

export interface AddOn {
  schema_version: number;
  id: string;
  display_name: string;
  matchers: { images?: string[]; charts?: string[] };
  support: { status: string; eol_date?: string; citations: string[] };
  compat?: AddOnCompat[];
  recommendation?: string;
}

export interface RegistryResponse {
  addons: AddOn[];
}
