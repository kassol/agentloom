# CodexLoom

CodexLoom governs a team of long-lived Agents while delegating their execution to an Agent Runtime. This context keeps the product's organization and collaboration language independent of any particular runtime.

## Language

**Agent**:
A long-lived governed subject with a stable identity, Profile, relationships, and one primary Thread.
_Avoid_: Bot, worker, runtime

**Owner**:
The human who governs the Agent team and supplies facts, decisions, or authorization that Agents cannot provide themselves.
_Avoid_: User, administrator, operator

**Agent Runtime**:
The execution system that advances an Agent's Thread through Turns without owning the Agent's identity or collaboration records.
_Avoid_: Provider, model, Agent

**Runtime Binding**:
The durable association between an Agent and its Agent Runtime's native conversation.
_Avoid_: Thread ID, provider binding

**Thread**:
The long-lived trajectory of one Agent's work, independent of whether its Agent Runtime calls the native conversation a thread or a session.
_Avoid_: Session, runtime session

**Turn**:
One bounded execution within a Thread that handles an accepted work item.
_Avoid_: Run, native turn

**Recovery Turn**:
A new Turn that continues an interrupted objective in the same Thread after reconciling the durable effects of its predecessor.
_Avoid_: Retried Turn, resumed Turn, replay

**Profile**:
The durable statement of an Agent's identity, Domain, and Scope.
_Avoid_: System prompt, runtime configuration

**Message**:
A durable, directed request or notification between Agents whose delivery, handling, and reply relationships remain visible to Loom.
_Avoid_: Prompt, runtime message

**Topic**:
A bounded shared coordination record for work that spans Agents, Turns, or waiting periods; it does not own a Thread or execute work.
_Avoid_: Project, chat room, workflow

**Responsible Agent**:
The Agent accountable for a Topic as a whole, including integrating participant work and returning the result.
_Avoid_: Participant, assignee

**Participant**:
An Agent with a bounded responsibility inside a Topic but no accountability for the Topic as a whole.
_Avoid_: Responsible Agent, observer

**Needs You**:
A durable request for an Owner fact, decision, or authorization that the responsible Agent cannot supply itself.
_Avoid_: Notification, Agent Inbox

**Approval**:
An Owner decision that allows or denies one proposed Agent Runtime tool action before it executes.
_Avoid_: Needs You, sandbox, notification
