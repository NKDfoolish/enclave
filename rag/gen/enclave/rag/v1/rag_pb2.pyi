from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class PingRequest(_message.Message):
    __slots__ = ("message",)
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    message: str
    def __init__(self, message: _Optional[str] = ...) -> None: ...

class PingResponse(_message.Message):
    __slots__ = ("message", "service")
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    SERVICE_FIELD_NUMBER: _ClassVar[int]
    message: str
    service: str
    def __init__(self, message: _Optional[str] = ..., service: _Optional[str] = ...) -> None: ...

class AnswerRequest(_message.Message):
    __slots__ = ("workspace_id", "visibilities", "question", "history", "request_id")
    WORKSPACE_ID_FIELD_NUMBER: _ClassVar[int]
    VISIBILITIES_FIELD_NUMBER: _ClassVar[int]
    QUESTION_FIELD_NUMBER: _ClassVar[int]
    HISTORY_FIELD_NUMBER: _ClassVar[int]
    REQUEST_ID_FIELD_NUMBER: _ClassVar[int]
    workspace_id: str
    visibilities: _containers.RepeatedScalarFieldContainer[str]
    question: str
    history: _containers.RepeatedCompositeFieldContainer[Turn]
    request_id: str
    def __init__(self, workspace_id: _Optional[str] = ..., visibilities: _Optional[_Iterable[str]] = ..., question: _Optional[str] = ..., history: _Optional[_Iterable[_Union[Turn, _Mapping]]] = ..., request_id: _Optional[str] = ...) -> None: ...

class Turn(_message.Message):
    __slots__ = ("role", "content")
    ROLE_FIELD_NUMBER: _ClassVar[int]
    CONTENT_FIELD_NUMBER: _ClassVar[int]
    role: str
    content: str
    def __init__(self, role: _Optional[str] = ..., content: _Optional[str] = ...) -> None: ...

class AnswerResponse(_message.Message):
    __slots__ = ("sources", "delta", "done")
    SOURCES_FIELD_NUMBER: _ClassVar[int]
    DELTA_FIELD_NUMBER: _ClassVar[int]
    DONE_FIELD_NUMBER: _ClassVar[int]
    sources: Sources
    delta: str
    done: Done
    def __init__(self, sources: _Optional[_Union[Sources, _Mapping]] = ..., delta: _Optional[str] = ..., done: _Optional[_Union[Done, _Mapping]] = ...) -> None: ...

class Sources(_message.Message):
    __slots__ = ("sources",)
    SOURCES_FIELD_NUMBER: _ClassVar[int]
    sources: _containers.RepeatedCompositeFieldContainer[Source]
    def __init__(self, sources: _Optional[_Iterable[_Union[Source, _Mapping]]] = ...) -> None: ...

class Source(_message.Message):
    __slots__ = ("number", "document_id", "title", "page", "section", "snippet")
    NUMBER_FIELD_NUMBER: _ClassVar[int]
    DOCUMENT_ID_FIELD_NUMBER: _ClassVar[int]
    TITLE_FIELD_NUMBER: _ClassVar[int]
    PAGE_FIELD_NUMBER: _ClassVar[int]
    SECTION_FIELD_NUMBER: _ClassVar[int]
    SNIPPET_FIELD_NUMBER: _ClassVar[int]
    number: int
    document_id: str
    title: str
    page: int
    section: str
    snippet: str
    def __init__(self, number: _Optional[int] = ..., document_id: _Optional[str] = ..., title: _Optional[str] = ..., page: _Optional[int] = ..., section: _Optional[str] = ..., snippet: _Optional[str] = ...) -> None: ...

class Done(_message.Message):
    __slots__ = ("cited",)
    CITED_FIELD_NUMBER: _ClassVar[int]
    cited: _containers.RepeatedScalarFieldContainer[int]
    def __init__(self, cited: _Optional[_Iterable[int]] = ...) -> None: ...
