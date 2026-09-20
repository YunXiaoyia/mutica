---
name: shared-references
description: ARIS 共享参考文档库。不是可直接调用的技能——它被各研究 skill 以 ../shared-references/*.md 相对路径引用；绑定到任何绑定了研究类 skill 的 agent，即可让这些引用生效。
---
# Shared references

This directory is a reference library shared by the ARIS research skills.
Skills link into it with relative paths (`../shared-references/<doc>.md`),
so it must be materialized as a sibling of the skill directories that
reference it. Do not invoke it as a skill.
