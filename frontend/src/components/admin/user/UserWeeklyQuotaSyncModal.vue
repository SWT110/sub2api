<template>
  <BaseDialog
    :show="show"
    :title="t('admin.users.weeklyQuotaSync.title')"
    width="wide"
    @close="emit('close')"
  >
    <div v-if="loading" class="py-12 text-center text-sm text-gray-500 dark:text-gray-400">
      {{ t('common.loading') }}
    </div>

    <div v-else class="space-y-6">
      <section class="space-y-4 border-b border-gray-200 pb-5 dark:border-dark-700">
        <div class="flex items-center justify-between gap-4">
          <label for="weekly-quota-sync-enabled" class="input-label mb-0">
            {{ t('admin.users.weeklyQuotaSync.enabled') }}
          </label>
          <Toggle
            id="weekly-quota-sync-enabled"
            v-model="form.enabled"
            :aria-label="t('admin.users.weeklyQuotaSync.enabled')"
            :disabled="saving"
          />
        </div>

        <div class="grid gap-4 sm:grid-cols-[minmax(0,1fr)_10rem]">
          <div>
            <label for="weekly-quota-sync-source" class="input-label">
              {{ t('admin.users.weeklyQuotaSync.sourceAccount') }}
            </label>
            <Select
              id="weekly-quota-sync-source"
              v-model="form.source_account_id"
              :options="sourceAccountOptions"
              :placeholder="t('admin.users.weeklyQuotaSync.sourcePlaceholder')"
              :disabled="saving"
              searchable
            />
            <p class="input-hint">{{ t('admin.users.weeklyQuotaSync.sourceHint') }}</p>
          </div>

          <div>
            <label for="weekly-quota-sync-interval" class="input-label">
              {{ t('admin.users.weeklyQuotaSync.pollInterval') }}
            </label>
            <input
              id="weekly-quota-sync-interval"
              v-model.number="form.poll_interval_seconds"
              type="number"
              min="60"
              max="3600"
              step="60"
              class="input w-full"
              :disabled="saving"
            />
            <p class="input-hint">{{ t('admin.users.weeklyQuotaSync.pollIntervalHint') }}</p>
          </div>
        </div>
      </section>

      <section class="space-y-4">
        <div class="flex flex-wrap items-center justify-between gap-3">
          <h3 class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t('admin.users.weeklyQuotaSync.status') }}
          </h3>
          <button
            type="button"
            class="btn btn-secondary"
            :disabled="checking || saving || checkDisabled"
            @click="handleCheck"
          >
            <Icon name="refresh" size="sm" :class="['mr-1.5', checking && 'animate-spin']" />
            {{ checking ? t('admin.users.weeklyQuotaSync.checking') : t('admin.users.weeklyQuotaSync.checkNow') }}
          </button>
        </div>

        <dl class="grid gap-x-6 gap-y-4 border-y border-gray-200 py-4 text-sm sm:grid-cols-2 dark:border-dark-700">
          <div>
            <dt class="text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.users.weeklyQuotaSync.observedResetAt') }}
            </dt>
            <dd class="mt-1 font-medium text-gray-900 dark:text-white">
              {{ displayTime(status?.state.observed_reset_at) }}
            </dd>
          </div>
          <div>
            <dt class="text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.users.weeklyQuotaSync.windowStart') }}
            </dt>
            <dd class="mt-1 font-medium text-gray-900 dark:text-white">
              {{ displayTime(status?.state.last_window_start) }}
            </dd>
          </div>
          <div>
            <dt class="text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.users.weeklyQuotaSync.lastCheckedAt') }}
            </dt>
            <dd class="mt-1 font-medium text-gray-900 dark:text-white">
              {{ displayTime(status?.state.last_checked_at) }}
            </dd>
          </div>
          <div>
            <dt class="text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.users.weeklyQuotaSync.lastTriggeredAt') }}
            </dt>
            <dd class="mt-1 font-medium text-gray-900 dark:text-white">
              {{ displayTime(status?.state.last_triggered_at) }}
            </dd>
          </div>
          <div>
            <dt class="text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.users.weeklyQuotaSync.lastAffectedUsers') }}
            </dt>
            <dd class="mt-1 font-medium text-gray-900 dark:text-white">
              {{ status?.state.last_affected_users ?? t('admin.users.weeklyQuotaSync.notAvailable') }}
            </dd>
          </div>
          <div v-if="status?.state.last_error" class="sm:col-span-2">
            <dt class="text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.users.weeklyQuotaSync.lastError') }}
            </dt>
            <dd class="mt-1 break-words text-sm text-red-600 dark:text-red-400">
              {{ status.state.last_error }}
            </dd>
          </div>
        </dl>
      </section>

      <section class="space-y-3 border-t border-gray-200 pt-5 dark:border-dark-700">
        <h3 class="text-sm font-medium text-gray-900 dark:text-white">
          {{ t('admin.users.weeklyQuotaSync.manualAnchor') }}
        </h3>
        <div class="flex flex-wrap items-end gap-3">
          <div class="min-w-0 flex-1 sm:max-w-xs">
            <label for="weekly-quota-sync-anchor" class="input-label">
              {{ t('admin.users.weeklyQuotaSync.manualAnchorTime') }}
            </label>
            <input
              id="weekly-quota-sync-anchor"
              v-model="manualAnchorStart"
              type="datetime-local"
              class="input w-full"
              :disabled="resetting"
            />
          </div>
          <button
            type="button"
            class="btn btn-secondary"
            :disabled="resetting || !manualAnchorStart"
            @click="handleManualReset"
          >
            <Icon name="refresh" size="sm" :class="['mr-1.5', resetting && 'animate-spin']" />
            {{ resetting ? t('admin.users.weeklyQuotaSync.resetting') : t('admin.users.weeklyQuotaSync.resetAll') }}
          </button>
        </div>
      </section>
    </div>

    <template #footer>
      <div class="flex justify-end gap-3">
        <button type="button" class="btn btn-secondary" @click="emit('close')">
          {{ t('common.cancel') }}
        </button>
        <button type="button" class="btn btn-primary" :disabled="saving || !canSave" @click="handleSave">
          {{ saving ? t('admin.users.weeklyQuotaSync.saving') : t('admin.users.weeklyQuotaSync.save') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type {
  UserWeeklyQuotaSyncConfig,
  UserWeeklyQuotaSyncSourceAccount,
  UserWeeklyQuotaSyncStatus
} from '@/api/admin/users'
import { formatDateTime } from '@/utils/format'
import { useAppStore } from '@/stores/app'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import Select from '@/components/common/Select.vue'
import type { SelectOption } from '@/components/common/Select.vue'
import Toggle from '@/components/common/Toggle.vue'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ close: [] }>()

const { t } = useI18n()
const appStore = useAppStore()

const loading = ref(false)
const saving = ref(false)
const checking = ref(false)
const resetting = ref(false)
const status = ref<UserWeeklyQuotaSyncStatus | null>(null)
const sourceAccounts = ref<UserWeeklyQuotaSyncSourceAccount[]>([])
const manualAnchorStart = ref('')
const form = reactive<UserWeeklyQuotaSyncConfig>({
  enabled: false,
  source_account_id: 0,
  poll_interval_seconds: 300
})

const sourceAccountOptions = computed<SelectOption[]>(() =>
  sourceAccounts.value.map((account) => ({
    value: account.id,
    label: `#${account.id} ${account.name || t('admin.users.weeklyQuotaSync.notAvailable')} (${account.status})`,
    disabled: !account.eligible
  }))
)

const validPollInterval = computed(() => {
  const value = Number(form.poll_interval_seconds)
  return Number.isInteger(value) && value >= 60 && value <= 3600
})

const canSave = computed(() =>
  validPollInterval.value && (!form.enabled || form.source_account_id > 0)
)

const configNeedsSave = computed(() => {
  const current = status.value?.config
  return !current
    || current.enabled !== form.enabled
    || current.source_account_id !== form.source_account_id
    || current.poll_interval_seconds !== Number(form.poll_interval_seconds)
})

const checkDisabled = computed(() =>
  configNeedsSave.value || (status.value?.config.source_account_id ?? 0) <= 0
)

function applyStatus(next: UserWeeklyQuotaSyncStatus) {
  status.value = next
  form.enabled = next.config.enabled
  form.source_account_id = next.config.source_account_id
  form.poll_interval_seconds = next.config.poll_interval_seconds
}

function displayTime(value?: string | null): string {
  return formatDateTime(value) || t('admin.users.weeklyQuotaSync.notAvailable')
}

function errorMessage(error: any, fallback: string): string {
  return error?.response?.data?.message || error?.response?.data?.detail || error?.message || fallback
}

function toRFC3339(local: string): string | undefined {
  if (!local) return undefined
  const parsed = new Date(local)
  if (Number.isNaN(parsed.getTime())) return undefined
  return parsed.toISOString()
}

async function load() {
  loading.value = true
  manualAnchorStart.value = ''
  try {
    const [nextStatus, accounts] = await Promise.all([
      adminAPI.users.getUserWeeklyQuotaSyncStatus(),
      adminAPI.users.listUserWeeklyQuotaSyncSourceAccounts()
    ])
    applyStatus(nextStatus)
    sourceAccounts.value = accounts
  } catch (error: any) {
    appStore.showError(errorMessage(error, t('admin.users.weeklyQuotaSync.loadFailed')))
  } finally {
    loading.value = false
  }
}

watch(
  () => props.show,
  (show) => {
    if (show) void load()
  }
)

async function handleSave() {
  if (!validPollInterval.value) {
    appStore.showError(t('admin.users.weeklyQuotaSync.invalidPollInterval'))
    return
  }
  if (form.enabled && form.source_account_id <= 0) {
    appStore.showError(t('admin.users.weeklyQuotaSync.sourceRequired'))
    return
  }

  saving.value = true
  try {
    const next = await adminAPI.users.updateUserWeeklyQuotaSyncConfig({
      enabled: form.enabled,
      source_account_id: form.source_account_id,
      poll_interval_seconds: Number(form.poll_interval_seconds)
    })
    applyStatus(next)
    appStore.showSuccess(t('admin.users.weeklyQuotaSync.saveSuccess'))
  } catch (error: any) {
    appStore.showError(errorMessage(error, t('admin.users.weeklyQuotaSync.saveFailed')))
  } finally {
    saving.value = false
  }
}

async function handleCheck() {
  if (configNeedsSave.value || (status.value?.config.source_account_id ?? 0) <= 0) {
    appStore.showError(t('admin.users.weeklyQuotaSync.checkRequiresSavedSource'))
    return
  }

  checking.value = true
  try {
    const result = await adminAPI.users.checkUserWeeklyQuotaSyncNow()
    applyStatus(result.status)
    if (result.reset_detected) {
      appStore.showSuccess(t('admin.users.weeklyQuotaSync.checkReset', { count: result.affected_users }))
    } else if (result.baseline_initialized) {
      appStore.showSuccess(t('admin.users.weeklyQuotaSync.checkBaseline'))
    } else {
      appStore.showSuccess(t('admin.users.weeklyQuotaSync.checkComplete'))
    }
  } catch (error: any) {
    appStore.showError(errorMessage(error, t('admin.users.weeklyQuotaSync.checkFailed')))
  } finally {
    checking.value = false
  }
}

async function handleManualReset() {
  const startAt = toRFC3339(manualAnchorStart.value)
  if (!startAt) {
    appStore.showError(t('admin.users.weeklyQuotaSync.invalidAnchorTime'))
    return
  }
  if (new Date(startAt).getTime() > Date.now()) {
    appStore.showError(t('admin.users.weeklyQuotaSync.futureAnchorTime'))
    return
  }
  if (!window.confirm(t('admin.users.weeklyQuotaSync.resetConfirm'))) return

  resetting.value = true
  try {
    const result = await adminAPI.users.resetAllUserWeeklyQuotaSyncAt(startAt)
    manualAnchorStart.value = ''
    appStore.showSuccess(t('admin.users.weeklyQuotaSync.resetSuccess', { count: result.affected_users }))
    const next = await adminAPI.users.getUserWeeklyQuotaSyncStatus()
    applyStatus(next)
  } catch (error: any) {
    appStore.showError(errorMessage(error, t('admin.users.weeklyQuotaSync.resetFailed')))
  } finally {
    resetting.value = false
  }
}
</script>
