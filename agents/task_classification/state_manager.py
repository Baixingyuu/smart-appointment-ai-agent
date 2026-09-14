"""
状态管理器

负责管理对话状态流转（CLASSIFY / TICKETING / CONSULT）。
"""

from config.constants import SharedState, StateEnum
from typing import Optional


class StateManager:
    """状态管理器"""

    def __init__(self, shared_state: Optional[SharedState] = None):
        self.state = shared_state or SharedState()

    def get_current_state(self) -> StateEnum:
        return self.state.value or StateEnum.CLASSIFY

    def set_state(self, new_state: StateEnum) -> None:
        old_state = self.state.value
        self.state.value = new_state
        print(f"状态转换: {old_state} -> {new_state}")

    def reset_to_classify(self) -> None:
        self.set_state(StateEnum.CLASSIFY)

    def should_classify(self) -> bool:
        current = self.get_current_state()
        return current == StateEnum.CLASSIFY or current is None

    def is_in_ticketing_flow(self) -> bool:
        return self.get_current_state() == StateEnum.TICKETING

    def is_in_consultation_flow(self) -> bool:
        return self.get_current_state() == StateEnum.CONSULT

    def transition_to_ticketing(self) -> None:
        self.set_state(StateEnum.TICKETING)

    def transition_to_consultation(self) -> None:
        self.set_state(StateEnum.CONSULT)

    def get_state_description(self) -> str:
        state = self.get_current_state()
        descriptions = {
            StateEnum.CLASSIFY: "任务分类状态 - 等待识别用户意图",
            StateEnum.TICKETING: "工单流程状态 - 正在处理工单请求",
            StateEnum.CONSULT: "咨询流程状态 - 正在处理咨询请求",
        }
        return descriptions.get(state, "未知状态")

    def can_transition_to(self, target_state: StateEnum) -> bool:
        current = self.get_current_state()
        allowed = {
            StateEnum.CLASSIFY: [StateEnum.TICKETING, StateEnum.CONSULT],
            StateEnum.TICKETING: [StateEnum.CLASSIFY],
            StateEnum.CONSULT: [StateEnum.CLASSIFY],
        }
        return target_state in allowed.get(current, [])

    def force_reset(self) -> None:
        print("强制重置状态到分类状态")
        self.reset_to_classify()
