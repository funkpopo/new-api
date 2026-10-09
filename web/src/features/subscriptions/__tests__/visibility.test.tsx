/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { SubscriptionsMutateDrawer } from '../components/subscriptions-mutate-drawer'
import { SubscriptionsProvider } from '../components/subscriptions-provider'
import {
  formValuesToPlanPayload,
  PLAN_FORM_DEFAULTS,
  planToFormValues,
} from '../lib/plan-form'
import type { PlanRecord, SubscriptionPlan } from '../types'

function renderPlanEditor(record?: PlanRecord) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <SubscriptionsProvider>
        <SubscriptionsMutateDrawer
          open
          onOpenChange={() => {}}
          currentRow={record}
        />
      </SubscriptionsProvider>
    </QueryClientProvider>
  )
}

beforeEach(() => {
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data: url === '/api/group' ? ['default', 'vip', 'partner'] : [],
    },
  }))
})

describe('subscription plan visibility', () => {
  it('submits selected groups when creating a plan using the keyboard', async () => {
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValue({ data: { success: true } })
    const user = userEvent.setup()
    renderPlanEditor()

    await user.type(screen.getByLabelText('Plan Title'), 'Restricted plan')
    const groups = screen.getByRole('combobox', { name: 'Visible Groups' })
    expect(groups).toHaveAttribute('placeholder', 'All Groups')
    expect(groups).toHaveAccessibleDescription(
      'Only users in the selected groups can view and purchase this plan. Leave empty for all groups.'
    )
    await user.click(groups)
    await user.type(groups, 'vip')
    await screen.findByRole('option', { name: 'vip' })
    await user.keyboard('{ArrowDown}')
    await waitFor(() =>
      expect(groups).toHaveAttribute(
        'aria-activedescendant',
        screen.getByRole('option', { name: 'vip' }).id
      )
    )
    expect(groups).toHaveFocus()
    await user.keyboard('{Enter}')
    expect(post).not.toHaveBeenCalled()
    expect(screen.getByText('vip')).toBeVisible()
    expect(groups).not.toHaveAttribute('placeholder')
    await user.type(groups, 'partner')
    await screen.findByRole('option', { name: 'partner' })
    await user.keyboard('{ArrowDown}')
    await waitFor(() => expect(groups).toHaveAttribute('aria-activedescendant'))
    await user.keyboard('{Enter}')
    expect(post).not.toHaveBeenCalled()
    expect(screen.getByText('partner')).toBeVisible()
    expect(groups).not.toHaveAttribute('placeholder')
    await user.tab()
    expect(groups).not.toHaveAttribute('placeholder')
    await user.click(screen.getByRole('button', { name: 'Save changes' }))

    await waitFor(() =>
      expect(post).toHaveBeenCalledWith(
        '/api/subscription/admin/plans',
        expect.objectContaining({
          plan: expect.objectContaining({ visible_groups: 'vip,partner' }),
        })
      )
    )
  })

  it('loads existing groups and clearing them saves unrestricted visibility', async () => {
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    const user = userEvent.setup()
    const plan = {
      ...formValuesToPlanPayload({
        ...PLAN_FORM_DEFAULTS,
        title: 'Existing plan',
        visible_groups: ['vip', 'partner'],
      }).plan,
      id: 9,
    } as SubscriptionPlan
    renderPlanEditor({ plan })

    const groups = screen.getByRole('combobox', { name: 'Visible Groups' })
    await user.click(groups)
    expect(await screen.findByRole('option', { name: 'vip' })).toHaveAttribute(
      'aria-selected',
      'true'
    )
    expect(screen.getByRole('option', { name: 'partner' })).toHaveAttribute(
      'aria-selected',
      'true'
    )
    await user.click(screen.getByRole('option', { name: 'vip' }))
    await user.click(screen.getByRole('option', { name: 'partner' }))
    await user.keyboard('{Escape}')
    expect(groups).toHaveAttribute('placeholder', 'All Groups')
    await user.click(screen.getByRole('button', { name: 'Save changes' }))

    await waitFor(() =>
      expect(put).toHaveBeenCalledWith(
        '/api/subscription/admin/plans/9',
        expect.objectContaining({
          plan: expect.objectContaining({ visible_groups: '' }),
        })
      )
    )
  })

  it.each([undefined, '', 'vip', 'vip,partner'])(
    'preserves legacy and configured visibility when editing %s',
    (visibleGroups) => {
      const plan = {
        ...formValuesToPlanPayload({ ...PLAN_FORM_DEFAULTS, title: 'Plan' })
          .plan,
        id: 1,
        visible_groups: visibleGroups,
      } as SubscriptionPlan
      expect(
        formValuesToPlanPayload(planToFormValues(plan)).plan.visible_groups
      ).toBe(visibleGroups ?? '')
    }
  )
})
