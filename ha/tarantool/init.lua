-- Схема кэша создается только на узле, который стал лидером
box.watch('box.status', function(_, status)
    if status.is_ro then
        return
    end
    box.schema.space.create('cache', {
        if_not_exists = true,
        format = {
            { name = 'key', type = 'string' },
            { name = 'value', type = 'string' },
            { name = 'expires', type = 'number' },
        },
    })
    box.space.cache:create_index('pk', { parts = { 'key' }, if_not_exists = true })
end)
