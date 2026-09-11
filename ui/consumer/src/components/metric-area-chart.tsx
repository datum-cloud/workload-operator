/**
 * Compact area chart over portal-formatted Prometheus series.
 *
 * Host `MetricChart` is portal-internal; this plugin bundles `@datum-cloud/datum-ui/chart`
 * + recharts instead. Height is inline so we do not depend on host Tailwind
 * arbitrary values.
 */
import {
  transformForRecharts,
  usePrometheusChart,
  type MetricFormat,
  type PrometheusTimeRange,
} from '../lib/prometheus';
import { ChartContainer, ChartTooltip, type ChartConfig } from '@datum-cloud/datum-ui/chart';
import { cn } from '@datum-cloud/datum-ui/utils';
import { useId, useMemo } from 'react';
import { Area, AreaChart, CartesianGrid, XAxis, YAxis } from 'recharts';

function formatAxisValue(value: number, format: MetricFormat): string {
  if (!Number.isFinite(value)) return '—';
  switch (format) {
    case 'bytes': {
      const units = ['B', 'KB', 'MB', 'GB', 'TB'];
      let n = value;
      let i = 0;
      while (n >= 1024 && i < units.length - 1) {
        n /= 1024;
        i += 1;
      }
      return `${n.toFixed(n >= 10 || i === 0 ? 0 : 1)} ${units[i]}`;
    }
    case 'percent':
      return `${(value * 100).toFixed(0)}%`;
    case 'requestsPerSecond':
      return value >= 10 ? `${value.toFixed(0)}/s` : `${value.toFixed(2)}/s`;
    case 'milliseconds':
    case 'milliseconds-auto':
      return value >= 1000 ? `${(value / 1000).toFixed(2)}s` : `${value.toFixed(0)}ms`;
    default:
      return value >= 10 ? value.toFixed(0) : value.toFixed(2);
  }
}

function formatTimeTick(timestamp: number): string {
  const date = new Date(timestamp);
  return date.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
}

export function MetricAreaChart({
  query,
  timeRange,
  title,
  format = 'number',
  color = 'var(--primary)',
  enabled = true,
  className,
  height = 224,
}: {
  query: string | undefined;
  timeRange: PrometheusTimeRange;
  title: string;
  format?: MetricFormat;
  color?: string;
  enabled?: boolean;
  className?: string;
  height?: number;
}) {
  const gradientId = useId().replace(/:/g, '');
  const { data, isLoading, error } = usePrometheusChart(query, timeRange, { enabled: enabled && !!query });
  const chartData = useMemo(() => (data ? transformForRecharts(data) : []), [data]);
  const seriesName = data?.series[0]?.name ?? 'value';

  const chartConfig: ChartConfig = {
    [seriesName]: { label: title, color },
  };

  return (
    <div
      className={cn('border-border flex flex-col overflow-hidden rounded-xl border', className)}
      data-testid={`compute-plugin-metric-chart-${title.toLowerCase().replace(/\s+/g, '-')}`}>
      <div className="flex items-center justify-between px-4 py-3">
        <span className="text-sm font-semibold">{title}</span>
      </div>
      <div className="px-2 pb-3" style={{ height }}>
        {isLoading ? (
          <div className="text-muted-foreground flex h-full items-center justify-center text-xs">
            Loading…
          </div>
        ) : error ? (
          <div className="text-muted-foreground flex h-full items-center justify-center text-xs">
            {error.status === 403 || error.status === 401
              ? "You don't have permission to view metrics"
              : 'Unable to load metrics'}
          </div>
        ) : chartData.length === 0 ? (
          <div className="text-muted-foreground flex h-full items-center justify-center text-xs">
            No data
          </div>
        ) : (
          <ChartContainer config={chartConfig} className="h-full w-full">
            <AreaChart data={chartData} margin={{ top: 8, right: 12, left: 4, bottom: 0 }}>
              <defs>
                <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
                  <stop offset="5%" stopColor={color} stopOpacity={0.25} />
                  <stop offset="95%" stopColor={color} stopOpacity={0} />
                </linearGradient>
              </defs>
              <CartesianGrid strokeDasharray="3 3" vertical={false} />
              <XAxis
                dataKey="timestamp"
                type="number"
                scale="time"
                domain={['dataMin', 'dataMax']}
                tickFormatter={formatTimeTick}
                tickLine={false}
                axisLine={false}
                minTickGap={24}
                tick={{ fill: 'var(--muted-foreground)', fontSize: 11 }}
              />
              <YAxis
                tickFormatter={(value: number) => formatAxisValue(value, format)}
                tickLine={false}
                axisLine={false}
                width={56}
                tick={{ fill: 'var(--muted-foreground)', fontSize: 11 }}
              />
              <ChartTooltip
                content={({ active, payload }) => {
                  if (!active || !payload?.length) return null;
                  const point = payload[0];
                  const value = Number(point.value);
                  const ts = Number(point.payload?.timestamp);
                  return (
                    <div className="border-border bg-background rounded-md border px-2 py-1 text-xs shadow-sm">
                      <div className="text-muted-foreground">{formatTimeTick(ts)}</div>
                      <div className="font-medium">{formatAxisValue(value, format)}</div>
                    </div>
                  );
                }}
              />
              <Area
                type="monotone"
                dataKey={seriesName}
                stroke={color}
                strokeWidth={1.5}
                fill={`url(#${gradientId})`}
                fillOpacity={1}
                dot={false}
                isAnimationActive={false}
              />
            </AreaChart>
          </ChartContainer>
        )}
      </div>
    </div>
  );
}

export function formatKpiValue(value: number | undefined, format: MetricFormat): string {
  if (value === undefined || !Number.isFinite(value)) return '—';
  return formatAxisValue(value, format);
}
