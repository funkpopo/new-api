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
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { toast } from 'sonner'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import type { UserSubscriptionRecord } from '@/features/subscriptions/types'
import { api } from '@/lib/api'

import { SubscriptionPlansCard } from '../components/subscription-plans-card'

let subscriptions: UserSubscriptionRecord[]

function renderSubscriptions(onSubscriptionChange = vi.fn()) {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  })
  return render(
    <QueryClientProvider client={client}>
      <SubscriptionPlansCard
        topupInfo={null}
        onSubscriptionChange={onSubscriptionChange}
      />
    </QueryClientProvider>
  )
}

beforeEach(() => {
  const now = Math.floor(Date.now() / 1000)
  subscriptions = [
    {
      subscription: {
        id: 1,
        user_id: 8,
        plan_id: 9,
        status: 'active',
        start_time: now - 3600,
        end_time: now + 86400,
        amount_total: 1000,
        amount_used: 200,
      },
    },
    {
      subscription: {
        id: 2,
        user_id: 8,
        plan_id: 9,
        status: 'cancelled',
        start_time: now - 3600,
        end_time: now - 60,
        amount_total: 1000,
        amount_used: 100,
      },
    },
    {
      subscription: {
        id: 3,
        user_id: 8,
        plan_id: 9,
        status: 'active',
        start_time: now - 3600,
        end_time: now - 1,
        amount_total: 1000,
        amount_used: 100,
      },
    },
  ]
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data:
        url === '/api/subscription/plans'
          ? []
          : {
              billing_preference: 'subscription_only',
              subscriptions: subscriptions.filter(
                (sub) =>
                  sub.subscription.status === 'active' &&
                  sub.subscription.end_time > Date.now() / 1000
              ),
              all_subscriptions: subscriptions,
            },
    },
  }))
})

describe('wallet subscription cancellation', () => {
  it('offers resubscription only for cancelled subscriptions that have not expired', async () => {
    subscriptions[1].subscription.end_time =
      subscriptions[0].subscription.end_time
    const post = vi.spyOn(api, 'post')
    const user = userEvent.setup()
    renderSubscriptions()

    const resubscribe = await screen.findByRole('button', {
      name: 'Resubscribe',
    })
    expect(screen.getAllByRole('button', { name: 'Resubscribe' })).toHaveLength(
      1
    )
    resubscribe.focus()
    await user.keyboard('{Enter}')
    const dialog = await screen.findByRole('alertdialog', {
      name: 'Resubscribe to subscription #2?',
    })
    expect(dialog).toHaveTextContent('original expiration date')
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    await waitFor(() =>
      expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    )
    expect(post).not.toHaveBeenCalled()
  })

  it('restores a cancelled subscription and refreshes account data without duplicate requests', async () => {
    subscriptions[1].subscription.end_time =
      subscriptions[0].subscription.end_time
    subscriptions[0].subscription.status = 'expired'
    let completeRequest!: (response: { data: { success: boolean } }) => void
    const pending = new Promise<{ data: { success: boolean } }>((resolve) => {
      completeRequest = resolve
    })
    const post = vi.spyOn(api, 'post').mockReturnValue(pending)
    const onChange = vi.fn()
    const user = userEvent.setup()
    renderSubscriptions(onChange)

    await user.click(await screen.findByRole('button', { name: 'Resubscribe' }))
    const dialog = await screen.findByRole('alertdialog')
    const confirm = within(dialog).getByRole('button', { name: 'Resubscribe' })
    await user.click(confirm)
    await waitFor(() => expect(confirm).toBeDisabled())
    expect(
      within(dialog).getByRole('button', { name: 'Cancel' })
    ).toBeDisabled()
    await user.click(confirm)
    expect(post).toHaveBeenCalledOnce()
    expect(post).toHaveBeenCalledWith('/api/subscription/self/2/resubscribe')

    subscriptions[1].subscription.status = 'active'
    await act(async () => completeRequest({ data: { success: true } }))
    await waitFor(() =>
      expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    )
    expect(
      screen.queryByRole('button', { name: 'Resubscribe' })
    ).not.toBeInTheDocument()
    expect(
      screen.getByRole('button', { name: 'Cancel subscription' })
    ).toBeEnabled()
    expect(screen.getByText('Active')).toBeVisible()
    expect(
      screen.queryByText(/Requests will be rejected/)
    ).not.toBeInTheDocument()
    expect(onChange).toHaveBeenCalledOnce()
  })

  it('offers cancellation only for active subscriptions and closing confirmation sends no request', async () => {
    const post = vi.spyOn(api, 'post')
    const user = userEvent.setup()
    renderSubscriptions()

    const cancel = await screen.findByRole('button', {
      name: 'Cancel subscription',
    })
    expect(
      screen.getAllByRole('button', { name: 'Cancel subscription' })
    ).toHaveLength(1)
    expect(
      screen.queryByRole('button', { name: 'Resubscribe' })
    ).not.toBeInTheDocument()
    cancel.focus()
    await user.keyboard('{Enter}')

    const dialog = await screen.findByRole('alertdialog', {
      name: 'Cancel subscription #1?',
    })
    expect(dialog).toHaveTextContent('without a refund')
    expect(dialog).toHaveTextContent('cancel it with that provider separately')
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    await waitFor(() =>
      expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    )
    expect(post).not.toHaveBeenCalled()
  })

  it('disables confirmation while pending and refreshes subscription and account data after cancellation', async () => {
    let completeRequest!: (response: { data: { success: boolean } }) => void
    const pending = new Promise<{ data: { success: boolean } }>((resolve) => {
      completeRequest = resolve
    })
    const post = vi.spyOn(api, 'post').mockReturnValue(pending)
    const onChange = vi.fn()
    const user = userEvent.setup()
    renderSubscriptions(onChange)

    await user.click(
      await screen.findByRole('button', { name: 'Cancel subscription' })
    )
    const dialog = await screen.findByRole('alertdialog')
    const confirm = within(dialog).getByRole('button', {
      name: 'Cancel subscription',
    })
    await user.click(confirm)
    await waitFor(() => expect(confirm).toBeDisabled())
    expect(
      within(dialog).getByRole('button', { name: 'Cancel' })
    ).toBeDisabled()
    await user.click(confirm)
    expect(post).toHaveBeenCalledOnce()
    expect(post).toHaveBeenCalledWith('/api/subscription/self/1/cancel')

    subscriptions[0].subscription.status = 'cancelled'
    await act(async () => completeRequest({ data: { success: true } }))
    await waitFor(() =>
      expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    )
    expect(
      screen.queryByRole('button', { name: 'Cancel subscription' })
    ).not.toBeInTheDocument()
    expect(screen.getByText('Subscription #1')).toBeVisible()
    expect(screen.getAllByText('Cancelled')).toHaveLength(2)
    expect(screen.getByRole('button', { name: 'Resubscribe' })).toBeVisible()
    expect(screen.getByText(/Requests will be rejected/)).toBeVisible()
    expect(onChange).toHaveBeenCalledOnce()
  })

  it.each([
    [
      'business failure',
      { data: { success: false, message: 'Subscription is no longer active' } },
    ],
    ['network failure', new Error('Connection unavailable')],
  ])(
    'keeps confirmation open and allows retry after %s',
    async (_, failure) => {
      const post = vi.spyOn(api, 'post')
      if (failure instanceof Error) post.mockRejectedValue(failure)
      else post.mockResolvedValue(failure)
      const errorToast = vi.spyOn(toast, 'error')
      const onChange = vi.fn()
      const user = userEvent.setup()
      renderSubscriptions(onChange)

      await user.click(
        await screen.findByRole('button', { name: 'Cancel subscription' })
      )
      const dialog = await screen.findByRole('alertdialog')
      const confirm = within(dialog).getByRole('button', {
        name: 'Cancel subscription',
      })
      await user.click(confirm)

      await waitFor(() => expect(errorToast).toHaveBeenCalledOnce())
      expect(dialog).toBeVisible()
      expect(confirm).toBeEnabled()
      expect(onChange).not.toHaveBeenCalled()
      expect(screen.getByText('Active')).toBeVisible()
    }
  )

  it.each([
    [
      'business failure',
      { data: { success: false, message: 'Subscription has expired' } },
    ],
    ['network failure', new Error('Connection unavailable')],
  ])(
    'keeps resubscription available for retry after %s',
    async (_, failure) => {
      subscriptions[1].subscription.end_time =
        subscriptions[0].subscription.end_time
      const post = vi.spyOn(api, 'post')
      if (failure instanceof Error) post.mockRejectedValue(failure)
      else post.mockResolvedValue(failure)
      const errorToast = vi.spyOn(toast, 'error')
      const onChange = vi.fn()
      const user = userEvent.setup()
      renderSubscriptions(onChange)

      await user.click(
        await screen.findByRole('button', { name: 'Resubscribe' })
      )
      const dialog = await screen.findByRole('alertdialog')
      const confirm = within(dialog).getByRole('button', {
        name: 'Resubscribe',
      })
      await user.click(confirm)

      await waitFor(() => expect(errorToast).toHaveBeenCalledOnce())
      expect(dialog).toBeVisible()
      expect(confirm).toBeEnabled()
      expect(screen.getByText('Cancelled')).toBeVisible()
      expect(onChange).not.toHaveBeenCalled()
    }
  )
})
