/**
 * Instance log surfaces built on `@datum-cloud/datum-ui/logs`.
 *
 * - {@link RecentInstanceLogs} — compact last-30-minutes table for Overview
 * - {@link InstanceLogsExplorer} — full explorer for the Logs tab
 *
 * v1 is ALB access logs only, and only when the workload has a published
 * HTTPProxy. Instance stdout is not in customer-facing o11y yet.
 */
import { ApiError } from '../lib/api';
import {
  albLogFacets,
  filterAlbLogsByHost,
  useAlbLogs,
  ALB_LOGS_PREVIEW_LIMIT,
} from '../lib/o11y-logs';
import { Card, CardContent } from '@datum-cloud/datum-ui/card';
import { EmptyContent } from '@datum-cloud/datum-ui/empty-content';
import { Icon } from '@datum-cloud/datum-ui/icons';
import {
  lastThirtyMinutes,
  Logs,
  resolveLogTimeRange,
  type LogColumnId,
  type LogFilters,
  type LogTimeRange,
} from '@datum-cloud/datum-ui/logs';
import { cn } from '@datum-cloud/datum-ui/utils';
import { ScrollTextIcon } from 'lucide-react';
import { useCallback, useMemo, useState } from 'react';
import { Link } from 'react-router';

const OVERVIEW_COLUMNS: readonly LogColumnId[] = ['time', 'status', 'path'];
const EXPLORER_COLUMNS: readonly LogColumnId[] = ['time', 'status', 'host', 'path'];

const UNPUBLISHED_TITLE = 'No load balancer logs';
const UNPUBLISHED_SUBTITLE =
  'ALB logs appear here when this workload is published on a public URL.';
const DENIED_MESSAGE = "You don't have permission to view load balancer logs.";

function UnpublishedLogs({ className }: { className?: string }) {
  return (
    <EmptyContent
      title={UNPUBLISHED_TITLE}
      subtitle={UNPUBLISHED_SUBTITLE}
      size="sm"
      variant="dashed"
      className={className}
    />
  );
}

/** Compact last-30-minutes log table for the instance Overview card. */
export function RecentInstanceLogs({
  logsHref,
  projectId,
  proxyId,
  className,
}: {
  logsHref: string;
  projectId?: string;
  proxyId?: string;
  className?: string;
}) {
  const [timeRange] = useState<LogTimeRange>(() => lastThirtyMinutes());
  const logsQuery = useAlbLogs(projectId, proxyId, {
    timeRange,
    limit: ALB_LOGS_PREVIEW_LIMIT,
    enabled: !!proxyId,
  });

  const denied = logsQuery.error instanceof ApiError && logsQuery.error.status === 403;
  const errorMessage =
    logsQuery.error && !denied ? logsQuery.error.message : undefined;

  return (
    <Card
      className={cn(
        'flex h-full w-full flex-col gap-0 overflow-hidden rounded-xl px-3 py-4 shadow sm:pt-6 sm:pb-4',
        className
      )}
      data-testid="compute-plugin-instance-logs">
      <CardContent className="flex min-h-0 flex-1 flex-col gap-3 p-0 sm:px-6 sm:pb-4">
        <div className="flex items-center justify-between gap-2">
          <div className="flex items-center gap-2.5">
            <Icon icon={ScrollTextIcon} size={20} className="text-muted-foreground" />
            <span className="text-base font-semibold">Recent Logs</span>
            <span className="text-muted-foreground text-xs">Last 30 min</span>
          </div>
          <Link
            to={logsHref}
            className="text-muted-foreground hover:text-foreground text-xs transition-colors">
            View all →
          </Link>
        </div>

        {!proxyId ? (
          <UnpublishedLogs className="min-h-48 flex-1 lg:min-h-72" />
        ) : denied ? (
          <EmptyContent
            title="Access restricted"
            subtitle={DENIED_MESSAGE}
            size="sm"
            variant="dashed"
            className="min-h-48 flex-1 lg:min-h-72"
          />
        ) : (
          <Logs.Root
            entries={logsQuery.data ?? []}
            isLoading={logsQuery.isLoading}
            error={errorMessage}
            columns={[...OVERVIEW_COLUMNS]}
            className="border-border flex min-h-48 flex-1 flex-col overflow-hidden rounded-lg border lg:min-h-72">
            <Logs.Table />
          </Logs.Root>
        )}
      </CardContent>
    </Card>
  );
}

/** Full log explorer for the instance Logs tab. */
export function InstanceLogsExplorer({
  projectId,
  proxyId,
  className,
}: {
  projectId?: string;
  proxyId?: string;
  className?: string;
}) {
  const [filters, setFilters] = useState<LogFilters>({});
  const [search, setSearch] = useState('');
  const [live, setLive] = useState(false);
  const [timeRange, setTimeRange] = useState<LogTimeRange>(() => lastThirtyMinutes());

  const handleRefresh = useCallback(() => {
    setTimeRange((current) =>
      current.preset ? resolveLogTimeRange(current) : lastThirtyMinutes()
    );
  }, []);

  const logsQuery = useAlbLogs(projectId, proxyId, {
    timeRange,
    filters,
    search,
    live,
    enabled: !!proxyId,
  });

  const visibleEntries = useMemo(
    () => filterAlbLogsByHost(logsQuery.data ?? [], filters),
    [logsQuery.data, filters]
  );
  const facets = useMemo(() => albLogFacets(logsQuery.data ?? []), [logsQuery.data]);

  const denied = logsQuery.error instanceof ApiError && logsQuery.error.status === 403;
  const errorMessage =
    logsQuery.error && !denied ? logsQuery.error.message : undefined;

  if (!proxyId) {
    return (
      <div
        className={cn('flex min-h-96 flex-col', className)}
        data-testid="compute-plugin-instance-logs-explorer">
        <UnpublishedLogs className="min-h-96 flex-1" />
      </div>
    );
  }

  if (denied) {
    return (
      <div
        className={cn('flex min-h-96 flex-col', className)}
        data-testid="compute-plugin-instance-logs-explorer">
        <EmptyContent
          title="Access restricted"
          subtitle={DENIED_MESSAGE}
          size="sm"
          variant="dashed"
          className="min-h-96 flex-1"
        />
      </div>
    );
  }

  return (
    <div
      className={cn(
        'border-border bg-card flex min-h-96 flex-col overflow-hidden rounded-xl border',
        className
      )}
      style={{ minHeight: '32rem' }}
      data-testid="compute-plugin-instance-logs-explorer">
      <Logs.Root
        entries={visibleEntries}
        facets={facets}
        timeRange={timeRange}
        filters={filters}
        search={search}
        live={live}
        isLoading={logsQuery.isLoading}
        error={errorMessage}
        columns={[...EXPLORER_COLUMNS]}
        onTimeRangeChange={setTimeRange}
        onFiltersChange={setFilters}
        onSearchChange={setSearch}
        onLiveChange={setLive}
        onRefresh={handleRefresh}
        className="bg-card flex min-h-0 flex-1 flex-col">
        <Logs.Explorer className="bg-card min-h-0 flex-1" />
      </Logs.Root>
    </div>
  );
}
