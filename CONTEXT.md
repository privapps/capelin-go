# Capelin Skill References

This context defines the language for selecting local skills from Capelin CLI requests.

## Language

**Skill reference**:
A `$name` notation in a local CLI request that selects a skill's guidance for that request; it does not itself execute the skill.
_Avoid_: Skill command, skill invocation

**Selected skill**:
A skill identified by a valid skill reference and made available as task-specific guidance for the request.
_Avoid_: Active skill, running skill

**Skill execution**:
Running a command declared by a skill through Capelin's ordinary tool and permission policy.
_Avoid_: Skill reference, skill selection
