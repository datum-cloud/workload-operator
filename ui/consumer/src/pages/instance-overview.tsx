/**
 * Instance Overview tab body. Mounted under {@link ./instance-detail.tsx}'s
 * layout shell via `<Outlet />` — chrome stays mounted across tab changes.
 */
import { CommandBlock } from '../components/cli-section';
import { DetailList, StatusBadge } from '../components/detail-list';
import { RecentInstanceLogs } from '../components/instance-logs';
import { formatKpiValue } from '../components/metric-area-chart';
import { useInstanceOutlet } from './instance-outlet-context';
import {
  albErrorRateQuery,
  albP99Query,
  albRpsQuery,
  cpuUsageQuery,
  memoryUsageQuery,
  useInstanceMetricIdentity,
} from '../lib/metrics-queries';
import { usePrometheusCard } from '../lib/prometheus';
import { instanceStatusToBadgeType, type Instance } from '../schema';
import { Card, CardContent } from '@datum-cloud/datum-ui/card';
import { useCopyToClipboard } from '@datum-cloud/datum-ui/hooks';
import { Icon } from '@datum-cloud/datum-ui/icons';
import { toast } from '@datum-cloud/datum-ui/toast';
import { cn } from '@datum-cloud/datum-ui/utils';
import {
  ChartColumnIncreasingIcon,
  CheckIcon,
  CopyIcon,
  SquareLibraryIcon,
  SquareTerminalIcon,
} from 'lucide-react';
import { useState } from 'react';
import { Link } from 'react-router';

const COMING_SOON = 'Coming soon';

/** Minimal local stand-in for the portal's internal `TextCopy` / `BadgeCopy`. */
function CopyableText({
  value,
  text,
  className,
  textClassName,
}: {
  value: string;
  text?: string;
  className?: string;
  textClassName?: string;
}) {
  const [, copy] = useCopyToClipboard();
  const [copied, setCopied] = useState(false);
  const display = text ?? value;

  return (
    <button
      type="button"
      title={display}
      onClick={() =>
        copy(value).then(() => {
          toast.success('Copied to clipboard');
          setCopied(true);
          setTimeout(() => setCopied(false), 2000);
        })
      }
      className={cn('inline-flex max-w-full min-w-0 items-center gap-1.5 text-left', className)}>
      <span className={cn('min-w-0 truncate', textClassName)}>{display}</span>
      {copied ? (
        <Icon icon={CheckIcon} size={14} className="shrink-0" />
      ) : (
        <Icon icon={CopyIcon} size={14} className="shrink-0 opacity-60" />
      )}
    </button>
  );
}

function ComingSoonValue() {
  return <span className="text-muted-foreground">{COMING_SOON}</span>;
}

function formatCpu(cpu?: string): string | undefined {
  if (!cpu) return undefined;
  if (/^\d+(\.\d+)?$/.test(cpu)) {
    const n = Number(cpu);
    return `${cpu} ${n === 1 ? 'core' : 'cores'}`;
  }
  return cpu;
}

function formatMemory(memory?: string): string | undefined {
  if (!memory) return undefined;
  const gi = memory.match(/^(\d+(?:\.\d+)?)Gi$/i);
  if (gi) return `${gi[1]} GiB`;
  const mi = memory.match(/^(\d+(?:\.\d+)?)Mi$/i);
  if (mi) return `${mi[1]} MiB`;
  return memory;
}

function formatCreatedAt(date: Date): string {
  return date.toLocaleDateString('en-GB', {
    day: '2-digit',
    month: 'short',
    year: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  });
}

function GeneralCard({ instance }: { instance: Instance }) {
  const hostname = instance.externalIP;
  const cpu = formatCpu(instance.cpu);
  const memory = formatMemory(instance.memory);

  return (
    <Card
      className="w-full gap-0 overflow-hidden rounded-xl px-3 py-4 shadow sm:pt-6 sm:pb-4"
      data-testid="compute-plugin-instance-general">
      <CardContent className="p-0 sm:px-6 sm:pb-4">
        <div className="mb-4 flex items-center gap-2.5">
          <Icon icon={SquareLibraryIcon} size={20} className="text-muted-foreground" />
          <span className="text-base font-semibold">General</span>
        </div>
        <DetailList
          items={[
            {
              label: 'Status',
              content: (
                <StatusBadge type={instanceStatusToBadgeType(instance.status)}>
                  {instance.status}
                </StatusBadge>
              ),
            },
            {
              label: 'Resource Name',
              content: <CopyableText value={instance.name} className="font-mono" />,
            },
            {
              label: 'Default Hostname',
              content: hostname ? (
                <CopyableText
                  value={hostname}
                  className="text-primary font-mono text-xs"
                  textClassName="max-w-[250px]"
                />
              ) : (
                <ComingSoonValue />
              ),
            },
            {
              label: 'Created At',
              content: formatCreatedAt(instance.createdAt),
            },
            {
              label: 'vCPU',
              content: cpu ?? <ComingSoonValue />,
            },
            {
              label: 'Memory',
              content: memory ?? <ComingSoonValue />,
            },
          ]}
        />
      </CardContent>
    </Card>
  );
}

function MetricsCard({
  projectId,
  instanceName,
  proxyId,
  metricsHref,
}: {
  projectId?: string;
  instanceName: string;
  proxyId?: string;
  metricsHref: string;
}) {
  const { identity, isLoading: identityLoading } = useInstanceMetricIdentity(projectId, instanceName);
  const enabled = !identityLoading && !!identity && !!projectId;
  const cpuQuery = enabled && identity && projectId ? cpuUsageQuery(projectId, identity) : undefined;
  const memoryQuery =
    enabled && identity && projectId ? memoryUsageQuery(projectId, identity) : undefined;
  const rpsQuery = projectId && proxyId ? albRpsQuery(projectId, proxyId) : undefined;
  const p99Query = projectId && proxyId ? albP99Query(projectId, proxyId) : undefined;
  const errorQuery = projectId && proxyId ? albErrorRateQuery(projectId, proxyId) : undefined;

  const cpu = usePrometheusCard(cpuQuery, 'number', { enabled });
  const memory = usePrometheusCard(memoryQuery, 'bytes', { enabled });
  const rps = usePrometheusCard(rpsQuery, 'requestsPerSecond', { enabled: !!proxyId });
  const p99 = usePrometheusCard(p99Query, 'milliseconds-auto', { enabled: !!proxyId });
  const errors = usePrometheusCard(errorQuery, 'percent', { enabled: !!proxyId });

  const kpis: Array<{ label: string; value: string }> = [
    { label: 'CPU', value: cpu.data?.formattedValue ?? formatKpiValue(cpu.data?.value, 'number') },
    { label: 'Memory', value: memory.data?.formattedValue ?? formatKpiValue(memory.data?.value, 'bytes') },
    {
      label: 'Requests',
      value: proxyId
        ? (rps.data?.formattedValue ?? formatKpiValue(rps.data?.value, 'requestsPerSecond'))
        : COMING_SOON,
    },
    {
      label: 'p99',
      value: proxyId
        ? (p99.data?.formattedValue ?? formatKpiValue(p99.data?.value, 'milliseconds-auto'))
        : COMING_SOON,
    },
    {
      label: 'Errors',
      value: proxyId
        ? (errors.data?.formattedValue ?? formatKpiValue(errors.data?.value, 'percent'))
        : COMING_SOON,
    },
  ];

  return (
    <Card
      className="w-full flex-1 gap-0 overflow-hidden rounded-xl px-3 py-4 shadow sm:pt-6 sm:pb-4"
      data-testid="compute-plugin-instance-metrics">
      <CardContent className="flex flex-col gap-4 p-0 sm:px-6 sm:pb-4">
        <div className="flex items-center gap-2.5">
          <Icon icon={ChartColumnIncreasingIcon} size={20} className="text-muted-foreground" />
          <span className="text-base font-semibold">Metrics</span>
        </div>
        <div className="divide-border border-border flex divide-x overflow-x-auto overscroll-x-contain rounded-lg border [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
          {kpis.map((kpi) => (
            <div key={kpi.label} className="flex min-w-24 flex-1 flex-col gap-1 px-3 py-3">
              <p className="text-muted-foreground text-xs font-medium tracking-wide uppercase">
                {kpi.label}
              </p>
              <p
                className={cn(
                  'text-xs whitespace-nowrap sm:text-sm',
                  kpi.value === COMING_SOON && 'text-muted-foreground'
                )}>
                {kpi.value}
              </p>
            </div>
          ))}
        </div>
        <div className="border-border bg-muted/40 text-muted-foreground flex h-36 items-center justify-center rounded-md border border-dashed text-xs">
          Network I/O — {COMING_SOON}
        </div>
        <Link
          to={metricsHref}
          className="text-muted-foreground hover:text-foreground text-xs transition-colors">
          View full metrics →
        </Link>
      </CardContent>
    </Card>
  );
}

export default function InstanceOverview() {
  const { instance, workloadName, logsHref, metricsHref, projectId, proxyId } = useInstanceOutlet();

  return (
    <>
      <div className="grid grid-cols-1 gap-6 lg:grid-cols-2">
        <div className="flex h-full flex-col gap-6">
          <GeneralCard instance={instance} />
          <MetricsCard
            projectId={projectId}
            instanceName={instance.name}
            proxyId={proxyId}
            metricsHref={metricsHref}
          />
        </div>
        <RecentInstanceLogs logsHref={logsHref} projectId={projectId} proxyId={proxyId} />
      </div>

      <Card
        className="w-full overflow-hidden rounded-xl px-3 py-4 shadow sm:pt-6 sm:pb-4"
        data-testid="compute-plugin-instance-cli">
        <CardContent className="p-0 sm:px-6 sm:pb-4">
          <div className="mb-4 flex flex-wrap items-center gap-2">
            <Icon icon={SquareTerminalIcon} size={20} className="text-muted-foreground" />
            <span className="text-base font-semibold">datumctl Commands</span>
            <span className="text-muted-foreground text-xs sm:ml-auto">CLI</span>
          </div>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div className="flex flex-col gap-1.5">
              <span className="text-muted-foreground text-xs">Get instance</span>
              <CommandBlock value={`datumctl compute instances get ${instance.name}`} />
            </div>
            <div className="flex flex-col gap-1.5">
              <span className="text-muted-foreground text-xs">List instances</span>
              <CommandBlock
                value={
                  workloadName
                    ? `datumctl compute instances list --workload=${workloadName}`
                    : 'datumctl compute instances list'
                }
              />
            </div>
            <div className="flex flex-col gap-1.5">
              <span className="text-muted-foreground text-xs">View logs</span>
              <CommandBlock value={`datumctl compute instances logs ${instance.name} --follow`} />
            </div>
            <div className="flex flex-col gap-1.5">
              <span className="text-muted-foreground text-xs">Describe</span>
              <CommandBlock
                value={`datumctl compute instances describe ${instance.name} --output yaml`}
              />
            </div>
          </div>
        </CardContent>
      </Card>
    </>
  );
}
