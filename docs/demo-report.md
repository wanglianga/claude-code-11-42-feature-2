# 演示报告

## 0. 报文契约注册

| 步骤 | 结果 | 说明 |
| --- | --- | --- |
| declaration/declaration | ✅ | registered v1 compat=strict fields=3 |
| scanner/scan | ✅ | registered v1 compat=backward fields=3 |
| screening/screening | ✅ | registered v1 compat=strict fields=4 |
| imaging/evidence | ✅ | registered v1 compat=backward fields=2 |
| review/manual_review | ✅ | registered v1 compat=strict fields=3 |
| stowage/container_stow | ✅ | registered v1 compat=backward fields=2 |
| manifest/flight_manifest | ✅ | registered v1 compat=backward fields=4 |
| custody/handover | ✅ | registered v1 compat=backward fields=3 |
| custody/package_change | ✅ | registered v1 compat=strict fields=4 |

## 1. 干净货件 AWB12345678901：暂定→正式分阶段发布

| 步骤 | 结果 | 说明 |
| --- | --- | --- |
| declaration | ✅ | 1 package declared for AWB 12345678901 |
| publish provisional @02:30 | ✅ | kind=provisional status=verified completeness=1.000 consumable=false |
| publish formal @03:00 | ✅ | kind=formal status=verified completeness=1.000 consumable=true diff=v1->v2, 3 new inputs |
| as-of publication 02:35 read | ✅ | v1 provisional status=verified |
| publish container AKE12345MU | ✅ | status=verified derived_from=1 shipment(s) |
| publish leg CA1234/2026-11-17/PEK-SHA | ✅ | status=verified derived_from=1 shipment(s) |
| replay bfab39db | ✅ | published=verified recomputed=verified match=true |
| replay 48e721dc | ✅ | published=verified recomputed=verified match=true |

## 2. 重复补传 + 拆包旧证据 AWB12345678902

| 步骤 | 结果 | 说明 |
| --- | --- | --- |
| identical retransmission | ✅ | outcome=duplicate_identical duplicate_of=4471eaba-d666-460a-aa7c-0ec777a6ea56 |
| conflicting retransmission | ✅ | outcome=duplicate_conflicting error=true (original 4471eaba-d666-460a-aa7c-0ec777a6ea56 kept) |
| review correction | ✅ | review supersedes device verdict 4471eaba |
| split at 04:25 | ✅ | PKG-DUP-1 -> PKG-SPLIT-1A + PKG-SPLIT-1B (new composition version, not a mutation) |
| late scan citing retired package | ✅ | arrived 6 minutes after the split; cites PKG-DUP-1 which no longer exists as a version |
| children re-screened | ✅ | both split children carry their own screening verdict + evidence archive |
| publish split shipment v1 | ✅ | status=pending_evidence completeness=0.833 consumable=false |
| publish split shipment v2 (after re-declaration) | ✅ | status=verified completeness=0.983 diff=v1->v2, status pending_evidence -> verified, 1 new inputs |
| quality after split | ✅ | 1 cross-package/misaligned findings |
| stale scan disposition | ✅ | disposition=superseded_package; event arrived after the package was retired by a split/merge; evidence is misaligned and is not attributed to any child |
| composition versions | ✅ | 2 versions: [v1(PKG-DUP-1) v2(G-SPLIT-1A+G-SPLIT-1B)] |
| replay split v1 | ✅ | published=pending_evidence recomputed=pending_evidence |
| replay split v2 | ✅ | published=verified recomputed=verified |

## 3. 跨来源冲突 AWB65432100003：冲突待裁定而非代选

| 步骤 | 结果 | 说明 |
| --- | --- | --- |
| publish conflict shipment | ✅ | status=conflict_pending conflicts=1 consumable=false |
| wait_complete with open sources | ✅ | skipped=true reason=wait_complete: watermark not closed (missing sources [declaration manifest scanner screening]); no snapshot published |
| forced formal publish | ✅ | kind=formal status=conflict_pending consumable=false |

## 4. 舱单缺证据 AWB98765400004：不得签发确认状态，只输出质量告警与待补证

| 步骤 | 结果 | 说明 |
| --- | --- | --- |
| publish manifest-gap shipment | ✅ | status=pending_evidence consumable=false pending=[departure_handover_missing manifest_required_evidence_missing] |

## 5. 包装谱系：来源范围、守恒核对、归属与修订影响

| 步骤 | 结果 | 说明 |
| --- | --- | --- |
| 混装再拆分（无逐件分配，仅投入/产出总数） | ✅ | A=10, B=6 -> X=8, Y=8 |
| 来源可达数量上下界（守恒约束求解） | ✅ | A->X [2,8], 缺失依据=[PED-OP-MIX] |
| 后续分配凭证收紧可行范围 | ✅ | A->X 收敛为 exact 5 |
| 嵌套装载查询（外箱与内件不重复计数） | ✅ | leaf packages=[PED-IN1 PED-IN2] |
| 异地同号包装保持不同实例 | ✅ | DUPNUM-1 @PEK and @SHA resolved separately |
| 互斥位置与合法先后换装 | ✅ | overlapping open stows flagged; handover-closed transload accepted |
| 皮重/毛重/容差核对 | ✅ | gross changed after rebox but net weight stays within tolerance |
| 相互矛盾的数量凭证 | ✅ | infeasible, conflicting ops=[PED-OP-BAD] |
| 迟到扫描归属（不按子件数量复制） | ✅ | parent scan=unattributable, scoped child scan=attributed |
| 早期操作更正的依赖影响分析 | ✅ | affected ops=[PED-OP-MIX], invalidated calcs=3, recomputable snapshots=[] |
| 旧版本计算的可重放性 | ✅ | v1 calc replayed to [2,8] despite v2 revision |
| pedigree scenario import | ✅ | ok |

## 5. 收尾校验：账本不可变 + 所有版本可复演 + 顶层约束

| 步骤 | 结果 | 说明 |
| --- | --- | --- |
| append-only guard | ✅ | UPDATE raw_events rejected by trigger: ERROR: table public.raw_events is append-only: UPDATE is not permitted (SQLSTATE P0001) |
| replay all snapshots | ✅ | 9/9 snapshots replay deterministically |
| status coverage | ✅ | map[conflict_pending:2 pending_evidence:2 verified:5] |

## 断言检查

| 检查 | 结果 | 依据 |
| --- | --- | --- |
| provisional published before sources closed | ✅ | early_provisional mode must publish at the cutoff even if sources are open |
| formal version follows provisional with explicit diff | ✅ | the second publication must be a new version superseding the provisional one |
| verified clean shipment is consumable-confirmed | ✅ | a formal verified snapshot is the only shape downstream may treat as confirmed |
| point-in-time query returns the then-readable provisional version | ✅ | as-of must reproduce what downstream could have read at that time |
| leg snapshot aggregates shipments across the leg | ✅ | the leg-level publication folds every shipment whose container flies that leg |
| clean shipment version replays deterministically | ✅ | every published version must be reproducible from its pinned inputs |
| clean shipment version replays deterministically | ✅ | every published version must be reproducible from its pinned inputs |
| identical retransmission suppressed as duplicate | ✅ | same idempotency content must not create a second event |
| conflicting retransmission quarantined, original retained | ✅ | a re-used idempotency key with new content must not overwrite |
| correction references the event it replaces | ✅ | supersedes relation is what makes the ruling auditable |
| split shipment does not silently inherit the retired parent's verdict | ✅ | the children were never covered by the original declaration, so v1 cannot be verified |
| re-declaration converges the split shipment in a new version | ✅ | post-split re-declaration is what makes the successor packages contributable |
| late old-package scan is misaligned, not auto-transferred | ✅ | a package retired by a split must not have its evidence silently attached to a child |
| stale scan appears in no child's evidence set | ✅ | the misaligned event must be retained for audit but absent from every successor's verdict inputs |
| split produced a new composition version | ✅ | the parent membership must be retired and the two children added as a new version |
| split shipment publication replays deterministically | ✅ | history including the split, the correction and the re-declaration must reproduce |
| split shipment publication replays deterministically | ✅ | history including the split, the correction and the re-declaration must reproduce |
| contradictory device results publish as conflict_pending | ✅ | without handover evidence the platform must not pick a verdict |
| conflict snapshot is not consumable-confirmed | ✅ | foreign systems must not treat a conflict as a confirmed status |
| wait_complete postpones publication until sources close | ✅ | the wait strategy deliberately holds the batch so no provisional value reaches downstream |
| even a forced formal snapshot keeps conflict_pending and is not consumable | ✅ | forcing the gate must never turn an unresolved conflict into a confirmed status |
| conflict snapshot replays deterministically | ✅ | the conflict set and decision must be reproducible |
| manifest citing missing required evidence cannot yield a confirmed status | ✅ | the snapshot must publish only quality warnings and pending-evidence items |
| manifest evidence gap is surfaced as a pending/blocking item | ✅ | a missing required evidence index must be visible to the operator |
| manifest-gap snapshot replays deterministically | ✅ | the refusal to confirm must be reproducible from history |
| 无分配时来源范围是区间而非确定份额 | ✅ | A->X = partial [2,8] |
| 开放区间给出缺失依据 | ✅ | missing_basis=[PED-OP-MIX] |
| 上界不可相加成来源总量 | ✅ | each boundary carries a full-chain witness |
| 补齐凭证后范围收敛为确定值 | ✅ | A->X after voucher = exact [5,5] |
| 嵌套容器只按叶节点汇总一次 | ✅ | leaves=[PED-IN1 PED-IN2] package_count=3 |
| 异地同号不并为一个实例 | ✅ | reused-label info finding present |
| 重叠位置判互斥、有交接的换装合法 | ✅ | mutex-position findings=5 |
| 换箱毛重不同但净重落在容差内 | ✅ | tare-aware weight conclusion within_tolerance |
| 矛盾凭证使全链不可行并指出冲突操作 | ✅ | status=infeasible conflicts=[PED-OP-BAD] |
| 包装件数不与内件数量直接相抵 | ✅ | package-item netting finding present |
| 拆分后迟到扫描不复制为多条子件确认 | ✅ | status=unattributable reason=scan cites a package retired before scan time with no current alias; it must not be copied to split children |
| 显式内容范围的扫描精确归属 | ✅ | status=attributed instance=PED-K1 |
| 修订只使依赖路径上的计算失效 | ✅ | affected=[PED-OP-MIX] invalidated=3 |
| 旧计算按固定操作版本可重算（新修订不替换当前归属） | ✅ | pinned=[2,8] replay=[2,8] |
| ledger rejects overwrite | ✅ | UPDATE on raw_events is blocked by the DB |
| all published versions replay deterministically | ✅ | each downstream state must be reproducible by historical version |

**41/41 checks passed**
