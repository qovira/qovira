<script lang="ts">
  import { Tooltip } from "@qovira/ui";
  import type { Component } from "svelte";

  interface Props {
    href: string;
    label: string;
    icon: Component<{ size?: number; "aria-hidden"?: "true" | "false" | boolean }>;
    active: boolean;
    expanded: boolean;
  }

  const { href, label, icon: Icon, active, expanded }: Props = $props();

  // Gate tooltip open state: when the rail is expanded the visible label makes
  // the tooltip redundant. We keep the Tooltip mounted regardless (unmounting
  // it would recreate the trigger anchor and drop keyboard focus). Instead we
  // prevent it from opening when the rail is already expanded.
  let tooltipOpen = $state(false);

  function handleTooltipOpenChange(open: boolean): void {
    // Allow the tooltip to open only when the rail is collapsed.
    tooltipOpen = expanded ? false : open;
  }
</script>

<!--
  The Tooltip wraps the anchor unconditionally so the anchor element is NEVER
  recreated when the rail expands/collapses. Focus is preserved across the
  expand transition because the same DOM node stays in place.
-->
<Tooltip side="right" sideOffset={8} bind:open={tooltipOpen} onOpenChange={handleTooltipOpenChange}>
  {#snippet trigger({ props })}
    <a
      {href}
      aria-current={active ? "page" : undefined}
      class="flex h-10 items-center gap-3 rounded px-2 text-sm font-medium transition-colors
             {active ? 'bg-accent/10 text-accent' : 'text-text hover:bg-surface-raised'}
             focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-current"
      {...props}
    >
      <Icon size={20} aria-hidden={true} />
      <!--
        Label is always in the DOM for a stable accessible name.
        When collapsed it is visually hidden (sr-only); when expanded it shows.
        Never toggled with {#if} so the anchor's accessible name is constant.
      -->
      <span class={expanded ? "block" : "sr-only"}>{label}</span>
    </a>
  {/snippet}
  {label}
</Tooltip>
