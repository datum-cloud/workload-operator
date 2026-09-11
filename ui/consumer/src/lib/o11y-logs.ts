/**
 * Project-scoped ALB access logs via telemetry queryapi's Loki-shaped route.
 *
 * Mirrors cloud-portal `app/resources/o11y-logs` without importing portal
 * internals. Identity is the HTTPProxy name (`route_name` regexp); search and
 * host filters stay client-side because Envoy OTEL access logs keep an empty
 * Body.
 */
import { ApiError, PLUGIN_ID, getProjectScopedBase } from './api';
import { useQuery, type UseQueryResult } from '@tanstack/react-query';
import {
  buildLogQL,
  facetsFromEntries,
  filterEntries,
  flattenLokiStreams,
  lastThirtyMinutes,
  logRequestHost,
  resolveLogTimeRange,
  type LogEntry,
  type LogFacet,
  type LogFilters,
  type LogTimeRange,
  type LokiQueryRangeResponse,
} from '@datum-cloud/datum-ui/logs';

export const O11Y_LOGS_QUERY_RANGE_PATH =
  '/apis/o11y.miloapis.com/v1alpha1/logs/loki/api/v1/query_range';

export const ALB_LOGS_PAGE_LIMIT = 500;
export const ALB_LOGS_PREVIEW_LIMIT = 20;
export const ALB_LOGS_LIVE_POLL_MS = 5_000;

const ALB_LOG_LABELS = [
  'method',
  'path',
  'response_code',
  'duration',
  'authority',
  'requested_server_name',
  'x_forwarded_host',
  'referer',
  'request_id',
  'protocol',
  'user_agent',
  'upstream_host',
  'response_flags',
] as const;

const ALB_LOG_LABEL_SET = new Set<string>(ALB_LOG_LABELS);

const CLIENT_HOST_FILTERS = [
  'host',
  'authority',
  'requested_server_name',
  'x_forwarded_host',
  'resource_name',
] as const;

export const ALB_LOG_FACET_NAMES = ['method', 'response_code', 'host'] as const;

export const ALB_LOG_FACET_LABELS: Record<(typeof ALB_LOG_FACET_NAMES)[number], string> = {
  method: 'Method',
  response_code: 'Status code',
  host: 'Host',
};

const LOKI_LABEL_ESCAPE = /[.*+?^${}()|[\]\\]/g;

function escapeLogQLQuoted(value: string): string {
  return value.replace(/\\/g, '\\\\').replace(/"/g, '\\"');
}

function escapeLogQLRegexp(value: string): string {
  return value.replace(LOKI_LABEL_ESCAPE, '\\$&');
}

/** Envoy `%ROUTE_NAME%` is `httproute/<namespace>/<httpProxyName>/rule/...`. */
export function albRouteNameRegexp(proxyId: string): string {
  return `httproute/[^/]+/${escapeLogQLRegexp(proxyId)}/.*`;
}

export function albLogMatchers(_proxyId: string, extraFilters: LogFilters = {}): LogFilters {
  const {
    resource_name: _ignoredResource,
    route_name: _ignoredRoute,
    host: _ignoredHost,
    authority: _ignoredAuthority,
    requested_server_name: _ignoredSni,
    x_forwarded_host: _ignoredXfh,
    ...rest
  } = extraFilters;
  return rest;
}

export function buildAlbLogQL(proxyId: string, extraFilters?: LogFilters): string {
  const extras = albLogMatchers(proxyId, extraFilters);
  const pin = `route_name=~"${escapeLogQLQuoted(albRouteNameRegexp(proxyId))}"`;
  const hasExtras = Object.values(extras).some((values) => values.length > 0);
  if (!hasExtras) return `{${pin}}`;
  return `{${pin}, ${buildLogQL({ matchers: extras }).slice(1)}`;
}

function formatDurationLabel(raw: string): string {
  const trimmed = raw.trim();
  if (!trimmed || /[a-z]/i.test(trimmed)) return trimmed;
  return `${trimmed}ms`;
}

export function pickAlbLogLabels(labels: Record<string, string>): Record<string, string> {
  const picked: Record<string, string> = {};
  for (const [key, value] of Object.entries(labels)) {
    if (!value || !ALB_LOG_LABEL_SET.has(key)) continue;
    picked[key] = key === 'duration' ? formatDurationLabel(value) : value;
  }
  const host = logRequestHost(picked);
  if (!host) return picked;
  return { host, ...picked };
}

function toLogEntries(response: LokiQueryRangeResponse): LogEntry[] {
  return flattenLokiStreams(response).map((entry) => ({
    ...entry,
    labels: pickAlbLogLabels(entry.labels),
  }));
}

export function albHostValues(labels: Record<string, string>): string[] {
  const seen = new Set<string>();
  const values: string[] = [];
  for (const raw of [
    labels.host,
    labels.requested_server_name,
    labels.x_forwarded_host,
    labels.authority,
  ]) {
    const host = raw?.trim();
    if (!host || seen.has(host)) continue;
    seen.add(host);
    values.push(host);
  }
  return values;
}

export function filterAlbLogsByHost(
  entries: readonly LogEntry[],
  filters: LogFilters = {}
): LogEntry[] {
  const selected = new Set<string>();
  for (const key of CLIENT_HOST_FILTERS) {
    for (const value of filters[key] ?? []) {
      if (value) selected.add(value);
    }
  }
  if (selected.size === 0) return [...entries];
  return entries.filter((entry) => albHostValues(entry.labels).some((host) => selected.has(host)));
}

function withAlbHostLabel(entry: LogEntry): LogEntry {
  const host = logRequestHost(entry.labels);
  if (!host || entry.labels.host === host) return entry;
  return { ...entry, labels: { ...entry.labels, host } };
}

export function albLogFacets(entries: readonly LogEntry[]): LogFacet[] {
  const hosted = entries.map(withAlbHostLabel);
  const byName = new Map(
    facetsFromEntries(hosted, ['method', 'response_code']).map((facet) => [facet.name, facet])
  );
  const hostFacet = hostFacetFromEntries(hosted);
  return ALB_LOG_FACET_NAMES.flatMap((name) => {
    if (name === 'host') return hostFacet ? [hostFacet] : [];
    const facet = byName.get(name);
    if (!facet) return [];
    return [{ ...facet, label: ALB_LOG_FACET_LABELS[name] }];
  });
}

function hostFacetFromEntries(entries: readonly LogEntry[]): LogFacet | null {
  const counts = new Map<string, number>();
  for (const entry of entries) {
    for (const host of albHostValues(entry.labels)) {
      counts.set(host, (counts.get(host) ?? 0) + 1);
    }
  }
  if (counts.size === 0) return null;
  const options = [...counts.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([value, count]) => ({ value, count }));
  return { name: 'host', label: ALB_LOG_FACET_LABELS.host, options };
}

async function queryRange(params: {
  projectId: string;
  query: string;
  start: string;
  end: string;
  limit: number;
}): Promise<LogEntry[]> {
  const search = new URLSearchParams({
    query: params.query,
    start: params.start,
    end: params.end,
    limit: String(params.limit),
    direction: 'backward',
  });
  const url = `${getProjectScopedBase(params.projectId)}${O11Y_LOGS_QUERY_RANGE_PATH}?${search.toString()}`;
  const res = await fetch(url, { headers: { Accept: 'application/json' } });
  if (!res.ok) {
    throw new ApiError(res.status, `Log query failed (${res.status})`);
  }
  const body = (await res.json()) as LokiQueryRangeResponse;
  if (body.status === 'error') {
    throw new ApiError(400, body.error || 'Log query failed');
  }
  return toLogEntries(body);
}

export interface UseAlbLogsOptions {
  timeRange: LogTimeRange;
  filters?: LogFilters;
  search?: string;
  live?: boolean;
  limit?: number;
  enabled?: boolean;
}

export function useAlbLogs(
  projectId: string | undefined,
  proxyId: string | undefined,
  options: UseAlbLogsOptions
): UseQueryResult<LogEntry[], ApiError> {
  const {
    timeRange,
    filters,
    search,
    live = false,
    limit = ALB_LOGS_PAGE_LIMIT,
    enabled = true,
  } = options;

  const query = proxyId ? buildAlbLogQL(proxyId, filters) : '';
  const windowKey = live ? 'live' : `${timeRange.from}/${timeRange.to}`;

  return useQuery({
    queryKey: [PLUGIN_ID, 'o11y-logs', projectId, proxyId, query, windowKey, limit],
    enabled: enabled && !!projectId && !!proxyId,
    queryFn: () => {
      const range = live
        ? resolveLogTimeRange(timeRange.preset ? timeRange : lastThirtyMinutes())
        : timeRange;
      return queryRange({
        projectId: projectId as string,
        query,
        start: range.from,
        end: range.to,
        limit,
      });
    },
    select: (entries) => filterEntries(entries, {}, search),
    refetchInterval: live ? ALB_LOGS_LIVE_POLL_MS : false,
    staleTime: live ? 0 : 15_000,
    retry: (failureCount, error) => {
      if (error instanceof ApiError && (error.status === 401 || error.status === 403 || error.status === 400)) {
        return false;
      }
      return failureCount < 2;
    },
  });
}
