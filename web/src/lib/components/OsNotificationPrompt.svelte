<script lang="ts">
  // Deferred OS-notification permission prompt.
  //
  // Rendered by the root layout when isPendingPermissionPrompt() is true (i.e. the first `reminder.fired` SSE
  // event arrived while Notification.permission is "default"). The prompt is:
  //   - Dismissible — clicking "Maybe later" calls dismissNotificationPrompt() and will not show again this
  //     session.
  //   - Actionable — clicking "Turn on notifications" calls requestOsNotificationPermission() then closes the
  //     prompt.
  //   - Keyboard-reachable — focus-trapped inside the banner via semantic buttons.
  //   - Branded — calm, unintrusive tone; no alarm or urgency.
  //   - Accessible — role="status" / aria-live="polite" so screen readers announce it without interrupting
  //     ongoing activity.

  import { Button } from "@qovira/ui";
  import {
    notification_prompt_title,
    notification_prompt_body,
    notification_prompt_enable,
    notification_prompt_dismiss,
  } from "$lib/paraglide/messages.js";
  import {
    dismissNotificationPrompt,
    requestOsNotificationPermission,
  } from "$lib/notifications/reminder-fired.svelte.js";

  async function handleEnable(): Promise<void> {
    await requestOsNotificationPermission();
  }

  function handleDismiss(): void {
    dismissNotificationPrompt();
  }
</script>

<!--
  Calm, unintrusive permission banner — fixed to the bottom of the viewport so it does not displace page
  content. role="status" + aria-live="polite" lets assistive technology surface it without interrupting any
  ongoing announcement.
-->
<div
  role="status"
  aria-live="polite"
  aria-atomic="true"
  class="bg-surface border-border fixed bottom-4 left-1/2 z-50 flex w-full max-w-sm -translate-x-1/2 flex-col gap-3 rounded-lg border px-4 py-3 shadow-md"
>
  <div class="flex flex-col gap-1">
    <p class="text-fg text-sm font-medium">{notification_prompt_title()}</p>
    <p class="text-fg-muted text-sm">{notification_prompt_body()}</p>
  </div>
  <div class="flex gap-2">
    <Button variant="primary" onclick={handleEnable}>
      {notification_prompt_enable()}
    </Button>
    <Button variant="ghost" onclick={handleDismiss}>
      {notification_prompt_dismiss()}
    </Button>
  </div>
</div>
