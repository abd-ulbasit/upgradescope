// Wire types mirroring the Go server's JSON (internal/server, registry).
// Field names match the Go struct tags exactly — keep in sync by hand.
// Fields marked "newer servers" are rendered only when present.

export type Severity = "blocker" | "warning" | "info";

// Verdict: blocked on any blocker; unknown when nothing blocks but a
// required check could not run (see CapabilityGap.required); else ready.
export type Verdict = "ready" | "blocked" | "unknown";

// Report source: a stored evaluation, or a what-if computed on request
// (not stored, so it has no history and cannot be exported).
export type ReportSource = "stored" | "what-if";

export interface EvalSummary {
  target: string;
  score: number;
  ready: boolean;
  verdict?: Verdict;
  blockers: number;
  warnings: number;
  kbVersion: string;
  evaluatedAt: string;
  snapshotId?: number;
}

export interface Cluster {
  id: number;
  name: string;
  clusterUid: string;
  firstSeen: string;
  lastSeen: string;
  stale?: boolean; // newer servers: no push within the stale window
}

export interface CapabilityStatus {
  available: boolean;
  reason?: string;
}

export interface ClusterDetail extends Cluster {
  serverVersion?: string;
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

export interface SuppressedFinding extends Finding {
  reason: string;
  source: string;
  expires?: string;
}

export interface CapabilityGap {
  capability: string;
  reason: string;
  required?: boolean; // the gap makes the verdict unknown
  partial?: boolean; // newer servers: the check ran but skipped some of its inputs
  skipped?: string[]; // newer servers: what a partial check skipped
}

export interface TeamScore {
  score: number;
  ready: boolean;
  blockers: number;
  warnings: number;
}

// Report is GET /clusters/{id}/report: the engine report, per-team scores
// and what the report is (stored or what-if, of which snapshot).
export interface Report {
  clusterId: string;
  target: string;
  kbVersion: string;
  score: number;
  ready: boolean;
  verdict?: Verdict;
  findings: Finding[];
  notAssessed?: CapabilityGap[];
  suppressed?: SuppressedFinding[];
  teams?: Record<string, TeamScore>;
  evaluatedAt?: string;
  snapshotId?: number;
  source?: ReportSource;
  serverVersion?: string;
  notApplicable?: boolean; // the cluster already runs the target
  // newer servers: image repositories no add-on registry entry matches
  // (sorted, at most 200), and how many more the cap dropped
  unrecognizedImages?: string[];
  unrecognizedImagesOmitted?: number;
}

export interface ScorePoint {
  at: string;
  score: number;
  ready: boolean;
}

export interface FleetCell {
  score: number;
  ready: boolean;
  verdict?: Verdict;
  blockers: number;
  evaluatedAt?: string;
}

export interface FleetRow {
  clusterId: number;
  name: string;
  serverVersion?: string;
  cells: Record<string, FleetCell | null>;
  notApplicable?: string[]; // targets the cluster already runs
  lastSeen?: string; // newer servers
  stale?: boolean; // newer servers
}

export interface FleetResponse {
  targets: string[];
  // Default columns left out past the server's 16; absent when none are.
  targetsOmitted?: number;
  clusters: FleetRow[];
}

export interface FleetTeam {
  worstScore: number;
  blockers: number;
  clusters: string[]; // cluster names
}

export interface FleetTeamsSource {
  name: string;
  clusterId: number;
  source: ReportSource;
  evaluatedAt: string;
  snapshotId: number;
}

// FleetTeamsResponse is GET /fleet/teams?target=: every team's rollup
// across the clusters that were evaluated for the target.
export interface FleetTeamsResponse {
  target: string;
  teams: Record<string, FleetTeam>;
  evaluated: FleetTeamsSource[];
  missing: string[];
  notApplicable: string[];
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
