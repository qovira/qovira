<script lang="ts">
  import { Button } from "@qovira/ui";

  import { logout } from "$lib/auth/logout.js";
  import { settings_heading, settings_logout } from "$lib/paraglide/messages.js";

  let loggingOut = $state(false);

  async function handleLogout(): Promise<void> {
    loggingOut = true;
    try {
      await logout();
    } finally {
      // logout() navigates to /login on success; reset in case navigation is
      // interrupted so the control never sticks disabled.
      loggingOut = false;
    }
  }
</script>

<h1 class="text-text text-xl font-semibold">{settings_heading()}</h1>

<div class="mt-6">
  <Button variant="secondary" loading={loggingOut} onclick={handleLogout}>{settings_logout()}</Button>
</div>
