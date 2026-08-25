/*
 * Copyright 2025 RethinkDNS and its authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package com.celzero.bravedns.service

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Tests for [TailscaleManager.TsState] mapping and eligibility logic; these
 * mirror ipn.State values from tailscale.com/ipn (see /tailscale/engine.go).
 */
class TailscaleManagerStateTest {

    @Test
    fun `state ids mirror ipn State values`() {
        assertEquals(0, TailscaleManager.TsState.NO_STATE.id)
        assertEquals(1, TailscaleManager.TsState.IN_USE_OTHER_USER.id)
        assertEquals(2, TailscaleManager.TsState.NEEDS_LOGIN.id)
        assertEquals(3, TailscaleManager.TsState.NEEDS_MACHINE_AUTH.id)
        assertEquals(4, TailscaleManager.TsState.STOPPED.id)
        assertEquals(5, TailscaleManager.TsState.STARTING.id)
        assertEquals(6, TailscaleManager.TsState.RUNNING.id)
    }

    @Test
    fun `of maps every known id`() {
        TailscaleManager.TsState.entries.forEach { state ->
            assertEquals(state, TailscaleManager.TsState.of(state.id))
        }
    }

    @Test
    fun `of falls back to NoState for unknown ids`() {
        assertEquals(TailscaleManager.TsState.NO_STATE, TailscaleManager.TsState.of(-1))
        assertEquals(TailscaleManager.TsState.NO_STATE, TailscaleManager.TsState.of(99))
    }

    @Test
    fun `only Running is usable`() {
        assertTrue(TailscaleManager.TsState.RUNNING.isUsable)
        TailscaleManager.TsState.entries
            .filter { it != TailscaleManager.TsState.RUNNING }
            .forEach { assertFalse(it.isUsable) }
    }

    @Test
    fun `login states detected correctly`() {
        assertTrue(TailscaleManager.TsState.NEEDS_LOGIN.needsLogin)
        assertTrue(TailscaleManager.TsState.NEEDS_MACHINE_AUTH.needsLogin)
        TailscaleManager.TsState.entries
            .filter {
                it != TailscaleManager.TsState.NEEDS_LOGIN &&
                    it != TailscaleManager.TsState.NEEDS_MACHINE_AUTH
            }
            .forEach { assertFalse(it.needsLogin) }
    }
}
